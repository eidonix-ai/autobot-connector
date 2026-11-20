package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"autobot-connector/internal/wire"
)

const hostMemoryAllowanceKiB = 976562

var workImage, cdiDigest, workProfile string // Set when the local images are built.

type leaseRequest struct {
	GPUs     int       `json:"gpus,omitempty"`
	Lease    string    `json:"lease"`
	Deadline time.Time `json:"deadline"`
	Salvage  []string  `json:"salvage,omitempty"`
	Bundle   bool      `json:"bundle,omitempty"`
}

type commandFact struct {
	ExitCode *int `json:"exit_code"`
}

type lease struct {
	leaseRequest
	Devices         []string      `json:"devices,omitempty"`
	Container       string        `json:"container"`
	End             string        `json:"end,omitempty"`
	Commands        []commandFact `json:"commands,omitempty"`
	OOMBefore       *int64        `json:"oom_before"`
	OOMAfter        *int64        `json:"oom_after"`
	MemoryAtQuiesce *int64        `json:"memory_at_quiesce,omitempty"`
	OOMKilled       *bool         `json:"oom_killed"`
	SalvageTar      []byte        `json:"salvage_tar,omitempty"`
	SalvageError    string        `json:"salvage_error,omitempty"`
	pending         string
	loading, loaded bool
	removed         bool
}

type state struct {
	Leases   map[string]*lease `json:"leases,omitempty"`
	FreeGPUs int               `json:"free_gpus,omitempty"`
	Active   *lease            `json:"active,omitempty"`
	Last     *lease            `json:"last,omitempty"`
	Baseline int64             `json:"baseline_kib"`
	removed  []string
}

type runner struct {
	mu sync.Mutex
	engine
	state
	statePath string
	dirty     bool
	reason    string
	results   map[string]*lease
	cleaned   map[string]bool
	vram      map[string]int
	usedGPUs  map[string]bool
	free      int
}

func cgroup(id string) string {
	return "/host-cgroup/system.slice/docker-" + id + ".scope"
}

func readCounter(path, key string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == key {
			return strconv.ParseInt(f[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("%s missing from %s", key, path)
}

func available() (int64, error) { return readCounter("/proc/meminfo", "MemAvailable:") }

func formatSHA(s string) bool {
	hex, ok := strings.CutPrefix(s, "sha256:")
	return ok && len(hex) == 64 && strings.Trim(hex, "0123456789abcdef") == ""
}

func (r *runner) save() (err error) {
	defer func() {
		if err != nil {
			r.dirty, r.reason, r.free = true, err.Error(), 0
		}
	}()
	b, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.statePath+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(r.statePath+".tmp", r.statePath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(r.statePath))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (r *runner) reconcile() error {
	var info struct {
		Driver, CgroupDriver, CgroupVersion string
		LiveRestoreEnabled                  bool
	}
	if err := r.call("GET", "/info", nil, &info); err != nil {
		return err
	}
	if info.Driver != "overlayfs" || info.CgroupDriver != "systemd" || info.CgroupVersion != "2" || info.LiveRestoreEnabled {
		return fmt.Errorf("Docker stack outside stage 1 profile: %+v", info)
	}
	list, err := r.labelled()
	if err != nil {
		return err
	}
	if r.Last != nil && r.Last.Container != "" {
		r.removed = append(r.removed, r.Last.Container)
	}
	for _, c := range list {
		id := c.Labels[leaseLabel]
		if r.Leases[id] == nil {
			r.Leases[id] = &lease{leaseRequest: leaseRequest{Lease: id}, Container: c.ID}
		}
		if err := r.remove(c.ID); err != nil {
			return err
		}
		r.removed = append(r.removed, c.ID)
	}
	for _, j := range r.Leases {
		if j.Container != "" {
			r.removed = append(r.removed, j.Container)
		}
		r.complete(&lease{leaseRequest: j.leaseRequest, Devices: j.Devices, Container: j.Container, End: "lost"})
	}
	if err := r.save(); err != nil {
		return err
	}
	r.dirty = false
	return nil
}

func (r *runner) capacity() ([]string, error) {
	list, err := r.labelled()
	if err != nil {
		r.dirty = true
		return nil, err
	}
	for _, c := range list {
		j := r.Leases[c.Labels[leaseLabel]]
		if j == nil || (j.Container != c.ID && !(j.Container == "" && j.End == "lost")) {
			return nil, fmt.Errorf("leftover labelled container %s", c.ID)
		}
	}
	for _, id := range r.removed {
		if _, err := os.Stat(cgroup(id)); !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("job cgroup %s still present or unverifiable: %v", id, err)
		}
	}
	r.removed = nil
	var image struct {
		ID, Architecture string
		Config           struct{ Volumes map[string]any }
	}
	err = r.call("GET", "/images/"+workImage+"/json", nil, &image)
	if err != nil || image.ID != workImage || image.Architecture != runtime.GOARCH || len(image.Config.Volumes) != 0 {
		return nil, fmt.Errorf("pinned %s work image unavailable or declares volumes", runtime.GOARCH)
	}
	spec, err := readCDI(cdiDigest)
	if err != nil {
		return nil, err
	}
	busy, memory, err := gpuUsage()
	if err != nil {
		return nil, err
	}
	reserved := map[string]bool{}
	for _, j := range r.Leases {
		for _, id := range j.Devices {
			reserved[id] = true
		}
	}
	r.updateVRAM(busy, memory)
	free, err := spec.freeGPUs(reserved, busy, memory, r.vram)
	if err != nil {
		return nil, err
	}
	if len(free) == 0 && len(r.Leases) == 0 {
		return nil, errors.New("no idle GPU within its memory recovery limit")
	}
	if len(free) > 0 {
		if err := r.verifyRecovery(free, available); err != nil {
			return nil, err
		}
	}
	return free, nil
}

func (r *runner) verifyRecovery(free []string, hostMemory func() (int64, error)) error {
	if gpuModel == "gb10" {
		mem, err := hostMemory()
		if err != nil {
			return err
		}
		floor := r.Baseline - hostMemoryAllowanceKiB
		if mem < floor {
			return fmt.Errorf("MemAvailable %d KiB below recovery floor %d KiB", mem, floor)
		}
	}
	available := map[string]bool{}
	for _, device := range free {
		available[device] = true
	}
	for id, j := range r.results {
		released := true
		for _, device := range j.Devices {
			released = released && available[device]
		}
		if released {
			r.cleaned[id] = true
		}
	}
	return nil
}

func (r *runner) complete(j *lease) {
	delete(r.Leases, j.Lease)
	r.Last = j
	r.results[j.Lease] = j
	r.pruneResults()
}

// The runner owns this loop and all end transitions, independent of client streams.
func (r *runner) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.free = 0
	if r.dirty {
		if err := r.reconcile(); err != nil {
			r.reason = err.Error()
			return
		}
	}
	for _, j := range r.Leases {
		if !j.removed && j.End != "lost" {
			switch info, err := r.inspect(j.Container); {
			case err != nil || !info.State.Running:
				j.End = "lost"
			case j.End == "" && time.Now().Before(j.Deadline):
				continue
			default:
				if j.End == "" {
					j.End = "deadline"
				}
				if err := r.finish(j, info); err != nil {
					log.Printf("finish lease %s: %v", j.Lease, err)
					j.End = "lost"
				}
			}
		}
		if err := r.retire(j); err != nil {
			log.Printf("retire lease %s: %v", j.Lease, err)
		}
		if r.dirty {
			return
		}
	}
	r.reason = ""
	free, err := r.capacity()
	if err != nil {
		r.reason = err.Error()
	}
	r.free = len(free)
}

func (r *runner) start(in leaseRequest) (*lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Leases[in.Lease] != nil {
		return nil, errors.New("lease already active")
	}
	if r.dirty {
		return nil, fmt.Errorf("unhealthy: %s", r.reason)
	}
	free, err := r.capacity()
	if err != nil {
		r.free, r.reason = 0, err.Error()
		return nil, fmt.Errorf("unhealthy: %w", err)
	}
	count := wire.GPUCount(in.GPUs)
	if count > len(free) {
		return nil, fmt.Errorf("%d GPUs requested, %d available", count, len(free))
	}
	spec, err := readCDI(cdiDigest)
	if err != nil {
		return nil, err
	}
	if gpuModel == "gb10" {
		r.Baseline, err = available()
		if err != nil {
			return nil, err
		}
	}
	j, err := r.startLease(in, spec, free[:count])
	r.free = len(free) - count
	if r.dirty {
		r.free = 0
	}
	return j, err
}

func (r *runner) startLease(in leaseRequest, spec cdiSpec, devices []string) (*lease, error) {
	j := &lease{leaseRequest: in, Devices: devices}
	r.Leases[in.Lease] = j
	if err := r.save(); err != nil {
		delete(r.Leases, in.Lease)
		return nil, err
	}
	for _, device := range devices {
		r.usedGPUs[device] = true
	}
	var err error
	j.Container, err = r.create(workImage, in.Lease, j.Devices)
	if err == nil {
		err = r.call("POST", "/containers/"+j.Container+"/start", nil, nil)
	}
	if err == nil {
		var c container
		c, err = r.inspect(j.Container)
		if err == nil {
			err = r.checkProfile(c, spec, j.Devices)
		}
	}
	if err == nil {
		var oom int64
		oom, err = readCounter(filepath.Join(cgroup(j.Container), "memory.events"), "oom_kill")
		j.OOMBefore = &oom
	}
	if err == nil {
		err = r.save()
	}
	if err != nil {
		j.End = "lost"
		return nil, errors.Join(err, r.retire(j))
	}
	copy := *j
	return &copy, nil
}

func (r *runner) retire(j *lease) error {
	if !j.removed {
		// Docker can create a container even if its reply never reaches the runner.
		if j.Container == "" {
			list, err := r.labelled()
			if err != nil {
				r.dirty, r.reason = true, err.Error()
				return err
			}
			for _, c := range list {
				if c.Labels[leaseLabel] == j.Lease {
					j.Container = c.ID
					break
				}
			}
		}
		if j.Container != "" {
			if err := r.remove(j.Container); err != nil {
				j.End = "lost"
				return err
			}
		}
		j.removed = true
	}
	if j.Container != "" {
		_, err := os.Stat(cgroup(j.Container))
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	r.complete(j)
	return r.save()
}

func (r *runner) finish(j *lease, c container) error {
	path := cgroup(j.Container)
	salvageSafe := r.onlyKeeper(c)
	if !salvageSafe {
		if err := r.call("POST", "/containers/"+j.Container+"/kill?signal=SIGUSR1", nil, nil); err != nil {
			return err
		}
		for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(25 * time.Millisecond) {
			if r.onlyKeeper(c) {
				salvageSafe = true
				break
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(path, "memory.current")); err == nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			j.MemoryAtQuiesce = &value
		}
	}
	if oom, err := readCounter(filepath.Join(path, "memory.events"), "oom_kill"); err == nil {
		j.OOMAfter = &oom
	}
	if j.pending != "" {
		fact := commandFact{}
		for until := time.Now().Add(750 * time.Millisecond); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
			if code, err := r.exit(j.pending); err == nil {
				fact.ExitCode = &code
				break
			}
		}
		j.Commands = append(j.Commands, fact)
		j.pending = ""
	}
	if len(j.Salvage) > 0 {
		if salvageSafe {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			data, err := r.fixed(ctx, j.Container, append([]string{"/stage1", "export"}, j.Salvage...)...)
			cancel()
			j.SalvageTar = data
			if err != nil {
				j.SalvageError = err.Error()
			}
		} else {
			j.SalvageError = "workload did not quiesce before removal"
		}
	}
	if c, err := r.inspect(j.Container); err == nil {
		j.OOMKilled = &c.State.OOMKilled
	}
	return nil
}

func (d engine) onlyKeeper(c container) bool {
	var top struct{ Processes [][]string }
	err := d.call("GET", "/containers/"+c.ID+"/top?ps_args=-eo%20pid", nil, &top)
	return err == nil && len(top.Processes) == 1 && len(top.Processes[0]) == 1 && top.Processes[0][0] == strconv.Itoa(c.State.Pid)
}
