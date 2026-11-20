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
	job     *job
	paused  bool
}

func (l *Link) active() *job {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	return l.state.job
}

func (l *Link) latest() wire.Health {
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	return l.state.sample
}

func (l *Link) status(ctx context.Context) wire.Status {
	l.state.mu.Lock()
	sample, paused, active := l.state.sample, l.state.paused, l.state.job != nil
	l.state.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	var r struct{ Status string }
	err := l.runner.json(ctx, "GET", "/readiness", nil, &r)
	return wire.Status{Health: sample, Paused: paused, Ready: !active && !paused && !sample.At.IsZero() &&
		len(sample.Reasons) == 0 && err == nil && r.Status == "ready", Arch: runtime.GOARCH, CPUs: runtime.NumCPU(),
		CPUQuota: 100000, Memory: 512 << 20, PIDs: 48, Image: l.config.Image, Scratch: wire.ScratchCap}
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
	gpus := make(chan wire.GPU, 1)
	go readGPU(ctx, l.config.NVML, gpus)
	tick := time.NewTicker(health.Interval)
	defer tick.Stop()
	var gpu wire.GPU
	var gpuAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case gpu = <-gpus:
			gpuAt = time.Now()
		case now := <-tick.C:
			sample := wire.Health{At: now.UTC()}
			sample.TotalKiB, sample.AvailableKiB, _ = health.HostMemory()
			if now.Sub(gpuAt) <= 3*time.Second {
				sample.GPUs = []wire.GPU{gpu}
			}
			_, pauseErr := os.Stat(filepath.Join(l.config.State, "pause"))
			clear := os.Remove(filepath.Join(l.config.State, "clear")) == nil
			l.applySample(sample, !errors.Is(pauseErr, os.ErrNotExist), clear)
		}
	}
}

func (l *Link) applySample(sample wire.Health, paused, clear bool) {
	l.state.mu.Lock()
	j := l.state.job
	tracker := &l.state.tracker
	before := tracker.Checkpoint()
	sample = tracker.Read(sample, j != nil && j.working.Load(), clear)
	if after := tracker.Checkpoint(); after != before {
		if err := l.checkpoint(after); err != nil {
			sample.Reasons = append(sample.Reasons, "health checkpoint failed")
			tracker.Restore(before)
		}
	}
	l.state.sample, l.state.paused = sample, paused
	l.state.mu.Unlock()
	if j != nil && len(sample.Reasons) > 0 {
		j.interrupt("health", sample)
	}
}

func readGPU(ctx context.Context, path string, out chan<- wire.GPU) {
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

func sampleGPU(ctx context.Context, path string, out chan<- wire.GPU) error {
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
		var gpu wire.GPU
		if err := wire.Strict(r.Bytes(), &gpu); err != nil {
			c.Process.Kill()
			return errors.Join(err, c.Wait())
		}
		select {
		case out <- gpu:
		case <-ctx.Done():
			return c.Wait()
		}
	}
	return errors.Join(fmt.Errorf("NVML reader exited"), r.Err(), c.Wait())
}
