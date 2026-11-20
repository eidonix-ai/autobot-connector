package link

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"autobot-connector/internal/health"
	"autobot-connector/internal/wire"
)

type localState struct {
	mu      sync.Mutex
	tracker health.Tracker
	sample  wire.Health
	jobs    map[string]*job
	paused  bool
}

func (l *Link) active(id string) *job {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	return l.state.jobs[id]
}

func (l *Link) activeJobs() []*job {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	out := make([]*job, 0, len(l.state.jobs))
	for _, j := range l.state.jobs {
		out = append(out, j)
	}
	return out
}

func (l *Link) latest() wire.Health {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	return l.state.sample
}

func (l *Link) status(ctx context.Context) wire.Status {
	l.state.mu.Lock()
	sample, paused := l.state.sample, l.state.paused
	pending := 0
	for _, j := range l.state.jobs {
		if !j.working.Load() {
			pending += wire.GPUCount(j.offer.Manifest.GPUs)
		}
	}
	l.state.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	var r runnerReadiness
	err := l.runner.json(ctx, "GET", "/readiness", nil, &r)
	free := 0
	if err == nil && r.WorkProfile != nil && *r.WorkProfile == l.workProfile {
		if r.FreeGPUs != nil {
			free = max(0, *r.FreeGPUs-pending)
		} else if r.Status == "ready" && pending == 0 {
			free = 1
		}
	}
	free = min(free, len(sample.GPUs))
	limits, _ := wire.WorkLimits(l.workProfile)
	return wire.Status{Health: sample, Paused: paused, Ready: free > 0 && !paused && !sample.At.IsZero() &&
		len(sample.Reasons) == 0, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), FreeGPUs: &free,
		CPUQuota: limits.CPU * 100000, Memory: limits.Memory, PIDs: limits.PIDs, Image: l.config.Image, Scratch: limits.Scratch}
}

func (l *Link) restoreHealth() error {
	l.state.tracker.MaxTemperature = l.config.MaxTemperature
	b, err := os.ReadFile(filepath.Join(l.config.State, "health.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state health.Checkpoint
	if err := wire.Strict(b, &state); err != nil {
		return err
	}
	l.state.tracker.Restore(state)
	return nil
}

func (l *Link) checkpoint(s health.Checkpoint) error {
	path := filepath.Join(l.config.State, "health.json")
	b, err := json.Marshal(s)
	if err == nil {
		err = os.WriteFile(path+".tmp", b, 0600)
	}
	if err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (l *Link) monitor(ctx context.Context) {
	gpus := make(chan []wire.GPU, 1)
	go readGPU(ctx, l.config.NVML, gpus)
	tick := time.NewTicker(health.Interval)
	defer tick.Stop()
	var gpusSample []wire.GPU
	var gpuAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case gpusSample = <-gpus:
			gpuAt = time.Now()
		case now := <-tick.C:
			sample := wire.Health{At: now.UTC()}
			sample.TotalKiB, sample.AvailableKiB, _ = health.HostMemory()
			if now.Sub(gpuAt) <= 3*time.Second {
				sample.GPUs = gpusSample
			}
			_, pauseErr := os.Stat(filepath.Join(l.config.State, "pause"))
			clear := os.Remove(filepath.Join(l.config.State, "clear")) == nil
			l.applySample(sample, !errors.Is(pauseErr, os.ErrNotExist), clear)
		}
	}
}

func (l *Link) applySample(sample wire.Health, paused, clear bool) {
	l.state.mu.Lock()
	jobs := make([]*job, 0, len(l.state.jobs))
	working := false
	for _, j := range l.state.jobs {
		jobs = append(jobs, j)
		working = working || j.working.Load()
	}
	tracker := &l.state.tracker
	before := tracker.Checkpoint()
	sample = tracker.Read(sample, working, clear)
	if after := tracker.Checkpoint(); after != before {
		if err := l.checkpoint(after); err != nil {
			sample.Reasons = append(sample.Reasons, "health checkpoint failed")
			tracker.Restore(before)
		}
	}
	l.state.sample, l.state.paused = sample, paused
	l.state.mu.Unlock()
	if len(sample.Reasons) > 0 {
		for _, j := range jobs {
			j.interrupt("health", sample)
		}
	}
}

func readGPU(ctx context.Context, path string, out chan<- []wire.GPU) {
	for ctx.Err() == nil {
		if err := sampleGPU(ctx, path, out); err != nil && ctx.Err() == nil {
			log.Printf("NVML sample: %q", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func sampleGPU(ctx context.Context, path string, out chan<- []wire.GPU) error {
	c := exec.CommandContext(ctx, path)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err = c.Start(); err != nil {
		return err
	}
	r := bufio.NewScanner(stdout)
	r.Buffer(make([]byte, 4096), 4096)
	for r.Scan() {
		var gpus []wire.GPU
		if err := wire.Strict(r.Bytes(), &gpus); err != nil {
			c.Process.Kill()
			return errors.Join(err, c.Wait())
		}
		select {
		case out <- gpus:
		case <-ctx.Done():
			return c.Wait()
		}
	}
	return errors.Join(fmt.Errorf("NVML reader exited"), r.Err(), c.Wait())
}
