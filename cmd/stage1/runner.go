package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const statePath = "/state/lease.json"

var workImage, cdiDigest string // Set when the local images are built.

type leaseRequest struct {
	Lease    string    `json:"lease"`
	Deadline time.Time `json:"deadline"`
	Salvage  []string  `json:"salvage,omitempty"`
}

type commandFact struct {
	ExitCode *int `json:"exit_code"`
}

type lease struct {
	leaseRequest
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
}

type state struct {
	Active   *lease `json:"active,omitempty"`
	Last     *lease `json:"last,omitempty"`
	Baseline int64  `json:"baseline_kib"`
	removed  []string
}

type runner struct {
	mu sync.Mutex
	engine
	state
	dirty          bool
	reason         string
	profileFailure string
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

func gpuIdle() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/nvidia-smi", "--query-compute-apps=pid", "--format=csv").Output()
	if err != nil {
		return fmt.Errorf("GPU process query: %w", err)
	}
	if strings.TrimSpace(string(out)) != "pid" {
		return fmt.Errorf("GPU has compute processes: %q", out)
	}
	return nil
}

func formatSHA(s string) bool {
	hex, ok := strings.CutPrefix(s, "sha256:")
	return ok && len(hex) == 64 && strings.Trim(hex, "0123456789abcdef") == ""
}

func (r *runner) save() error {
	b, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(statePath+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(statePath+".tmp", statePath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(statePath))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (r *runner) lose(leaseID, id string) {
	if r.Active == nil {
		r.Active = &lease{leaseRequest: leaseRequest{Lease: leaseID}, Container: id}
	}
	if id != "" {
		r.removed = append(r.removed, id)
	}
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
		r.lose(c.Labels[leaseLabel], c.ID)
		if err := r.remove(c.ID); err != nil {
			return err
		}
	}
	if j := r.Active; j != nil {
		if j.Container != "" {
			r.removed = append(r.removed, j.Container)
		}
		r.Last = &lease{leaseRequest: j.leaseRequest, Container: j.Container, End: "lost"}
		r.Active = nil
	}
	if err := r.save(); err != nil {
		return err
	}
	r.dirty = false
	return nil
}

func (r *runner) ready() error {
	list, err := r.labelled()
	if err != nil {
		r.dirty = true
		return err
	}
	if len(list) > 0 {
		return fmt.Errorf("leftover labelled container %s", list[0].ID)
	}
	for _, id := range r.removed {
		if _, err := os.Stat(cgroup(id)); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("job cgroup %s still present or unverifiable: %v", id, err)
		}
	}
	r.removed = nil
	if r.profileFailure != "" {
		return errors.New(r.profileFailure)
	}
	var image struct {
		ID, Architecture string
		Config           struct{ Volumes map[string]any }
	}
	err = r.call("GET", "/images/"+workImage+"/json", nil, &image)
	if err != nil || image.ID != workImage || image.Architecture != "arm64" || len(image.Config.Volumes) != 0 {
		return errors.New("pinned arm64 work image unavailable or declares volumes")
	}
	if _, err := readCDI(cdiDigest); err != nil {
		return err
	}
	if err = gpuIdle(); err != nil {
		return err
	}
	mem, err := available()
	if err != nil {
		return err
	}
	if mem < r.Baseline-976562 {
		return fmt.Errorf("MemAvailable %d KiB below recovery floor %d KiB", mem, r.Baseline-976562)
	}
	return nil
}

// The runner owns this loop and all end transitions, independent of client streams.
func (r *runner) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirty {
		if err := r.reconcile(); err != nil {
			r.reason = err.Error()
			return
		}
	}
	if j := r.Active; j != nil {
		info, err := r.inspect(j.Container)
		if err != nil || !info.State.Running {
			r.dirty = true
			r.reason = "Docker or job keeper became unavailable"
			return
		}
		if !time.Now().Before(j.Deadline) && j.End == "" {
			j.End = "deadline"
		}
		if j.End == "" {
			return
		}
		r.reason = "verifying teardown"
		if err := r.finish(j); err != nil {
			r.reason = err.Error()
			return
		}
	}
	r.reason = ""
	if err := r.ready(); err != nil {
		r.reason = err.Error()
	}
}

func (r *runner) start(in leaseRequest) (*lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Active != nil {
		return nil, errors.New("lease still active")
	}
	if r.dirty {
		return nil, fmt.Errorf("unhealthy: %s", r.reason)
	}
	if err := r.ready(); err != nil {
		r.reason = err.Error()
		return nil, fmt.Errorf("unhealthy: %w", err)
	}
	spec, err := readCDI(cdiDigest)
	if err != nil {
		return nil, err
	}
	r.Baseline, err = available()
	if err != nil {
		return nil, err
	}
	j := &lease{leaseRequest: in}
	r.Active = j
	if err := r.save(); err != nil {
		r.Active = nil
		return nil, err
	}
	j.Container, err = r.create(workImage, in.Lease)
	if err == nil {
		err = r.call("POST", "/containers/"+j.Container+"/start", nil, nil)
	}
	if err == nil {
		var c container
		c, err = r.inspect(j.Container)
		if err == nil {
			err = r.checkProfile(c, spec)
			if err != nil {
				r.profileFailure = "job profile: " + err.Error()
			}
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
		r.dirty, r.reason = true, err.Error()
		return nil, err
	}
	copy := *j
	return &copy, nil
}

func (r *runner) finish(j *lease) error {
	c, err := r.inspect(j.Container)
	if err != nil {
		r.dirty = true
		return err
	}
	path := cgroup(j.Container)
	salvageSafe := r.onlyKeeper(c)
	if !salvageSafe {
		err = r.call("POST", "/containers/"+j.Container+"/kill?signal=SIGUSR1", nil, nil)
		if err != nil {
			r.dirty = true
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
			j.SalvageTar, err = r.fixed(ctx, j.Container, append([]string{"/stage1", "export"}, j.Salvage...)...)
			cancel()
			if err != nil {
				j.SalvageError = err.Error()
			}
		} else {
			j.SalvageError = "workload did not quiesce before removal"
		}
	}
	c, err = r.inspect(j.Container)
	if err == nil {
		j.OOMKilled = &c.State.OOMKilled
	}
	if err = r.remove(j.Container); err != nil {
		r.dirty = true
		return err
	}
	r.removed = append(r.removed, j.Container)
	r.Last, r.Active = j, nil
	return r.save()
}

func (d engine) onlyKeeper(c container) bool {
	var top struct{ Processes [][]string }
	err := d.call("GET", "/containers/"+c.ID+"/top?ps_args=-eo%20pid", nil, &top)
	return err == nil && len(top.Processes) == 1 && len(top.Processes[0]) == 1 && top.Processes[0][0] == strconv.Itoa(c.State.Pid)
}
