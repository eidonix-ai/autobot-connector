package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const gpuKind = "flare.local/gpu="

var gpuModel string // Set by the native image build.

func checkGPUModel(model, names string) error {
	var expected string
	switch model {
	case "gb10":
		expected = "NVIDIA GB10"
	case "rtx-pro-6000":
		expected = "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition"
	default:
		return fmt.Errorf("unknown GPU model %q", model)
	}
	for name := range strings.SplitSeq(names, "\n") {
		if strings.TrimSpace(name) != expected {
			return fmt.Errorf("GPU model %q requires %q, NVML reported %q", model, expected, name)
		}
	}
	return nil
}

func gpuQuery(fields string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/nvidia-smi", fields, "--format=csv,noheader,nounits").Output()
	return strings.TrimSpace(string(out)), err
}

func gpuUsage() (busy map[string]bool, memory map[string]int, err error) {
	out, err := gpuQuery("--query-compute-apps=gpu_uuid,pid")
	if err != nil {
		return nil, nil, fmt.Errorf("GPU process query: %w", err)
	}
	busy = map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		uuid, pid, ok := strings.Cut(line, ",")
		if p, e := strconv.Atoi(strings.TrimSpace(pid)); !ok || !strings.HasPrefix(uuid, "GPU-") || e != nil || p < 1 {
			return nil, nil, fmt.Errorf("invalid GPU process row %q", line)
		}
		busy[uuid] = true
	}
	if gpuModel == "gb10" {
		return busy, nil, nil
	}
	out, err = gpuQuery("--query-gpu=uuid,memory.used")
	if err != nil {
		return nil, nil, fmt.Errorf("GPU memory query: %w", err)
	}
	memory = map[string]int{}
	for line := range strings.SplitSeq(out, "\n") {
		uuid, used, ok := strings.Cut(line, ",")
		mib, e := strconv.Atoi(strings.TrimSpace(used))
		if !ok || !strings.HasPrefix(uuid, "GPU-") || e != nil || mib < 0 {
			return nil, nil, fmt.Errorf("invalid GPU memory row %q", line)
		}
		memory[uuid] = mib
	}
	return busy, memory, nil
}

func (r *runner) updateVRAM(busy map[string]bool, memory map[string]int) {
	for id, used := range memory {
		_, known := r.vram[id]
		if !busy[id] && !r.usedGPUs[id] && (known || len(r.Leases) == 0) {
			r.vram[id] = used
		}
	}
}

func (spec cdiSpec) freeGPUs(reserved, busy map[string]bool, memory, baseline map[string]int) ([]string, error) {
	var free []string
	for _, d := range spec.Devices {
		if d.Name == "all" || reserved[d.Name] || busy[d.Name] {
			continue
		}
		if memory != nil {
			mib, ok := memory[d.Name]
			if !ok {
				return nil, fmt.Errorf("missing device memory for %s", d.Name)
			}
			if floor, ok := baseline[d.Name]; !ok || mib > floor+64 {
				continue
			}
		}
		free = append(free, d.Name)
	}
	return free, nil
}

func deviceRequests(devices []string) []string {
	out := make([]string, len(devices))
	for i, device := range devices {
		out[i] = gpuKind + device
	}
	return out
}

func (r *runner) pruneResults() {
	if len(r.results) <= 16 {
		return
	}
	var earliest *lease
	for _, j := range r.results {
		if j == r.Last {
			continue
		}
		if earliest == nil || j.Deadline.Before(earliest.Deadline) {
			earliest = j
		}
	}
	delete(r.results, earliest.Lease)
	delete(r.cleaned, earliest.Lease)
}

func (r *runner) activeLease(id string) *lease {
	return r.Leases[id]
}

func (r *runner) snapshot(id string) state {
	s := r.state
	s.Leases = nil
	s.Active = nil
	s.FreeGPUs = r.free
	if id != "" {
		s.FreeGPUs = 0
		s.Active, s.Last = r.Leases[id], r.results[id]
		if s.Last == nil && r.Last != nil && r.Last.Lease == id {
			s.Last = r.Last
		}
		if r.dirty || !r.cleaned[id] {
			s.Last = nil
		}
		return s
	}
	if s.Last != nil && (r.dirty || !r.cleaned[s.Last.Lease]) {
		s.Last = nil
	}
	for _, j := range r.Leases {
		s.Active = j
		break
	}
	return s
}
