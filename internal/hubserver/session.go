package hubserver

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"autobot-connector/internal/wire"
)

type Event struct {
	Chunk *wire.Chunk `json:"chunk,omitempty"`
	Exit  *wire.Exit  `json:"exit,omitempty"`
	Error string      `json:"error,omitempty"`
}

type Result struct {
	Lease    string     `json:"lease"`
	Verified bool       `json:"verified"`
	Facts    wire.Facts `json:"facts"`
	Salvage  []byte     `json:"salvage,omitempty"`
	Error    string     `json:"error,omitempty"`
}

type session struct {
	hub    *Hub
	id     string
	key    ed25519.PublicKey
	conn   *wire.Socket
	mu     sync.Mutex
	closed chan struct{}
	status wire.Status
	active map[string]*lease
	last   *lease
}

type lease struct {
	timer             *time.Timer
	id                string
	offer             wire.Offer
	devices           []string
	record            *wire.Record
	audit             *os.File
	phase             string
	start             chan error
	done              chan struct{}
	events            chan Event
	number, output    int
	ending, requested string
	exits             []wire.CommandFact
	salvage           bytes.Buffer
	result            Result
}

func (s *session) disconnect(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return
	default:
		close(s.closed)
	}
	s.conn.Close()
	for _, l := range s.active {
		s.terminal(l, Result{Lease: l.id, Error: reason, Facts: wire.Facts{End: "lost", Health: s.status.Health}})
	}
}

func (s *session) run() {
	go func() {
		ticker := time.NewTicker(wire.Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-ticker.C:
				if s.conn.Send("heartbeat", "", struct{}{}) != nil {
					s.disconnect("heartbeat write failed")
					return
				}
			}
		}
	}()
	for {
		m, err := s.conn.Read()
		if err != nil {
			reason := "donor disconnected"
			if deadline, ok := err.(interface{ Timeout() bool }); ok && deadline.Timeout() {
				reason = "donor heartbeat timeout"
			}
			s.disconnect(reason)
			return
		}
		if err := s.receive(m); err != nil {
			s.disconnect("donor protocol: " + err.Error())
			return
		}
	}
}

func (l *lease) add(fromHub bool, kind string, v any) error {
	l.record.Add(fromHub, kind, v)
	return json.NewEncoder(l.audit).Encode(struct {
		FromHub bool   `json:"from_hub"`
		Type    string `json:"type"`
		Data    any    `json:"data"`
	}{fromHub, kind, v})
}

type savedResult struct {
	Donor  string `json:"donor"`
	Result Result `json:"result"`
}

func (h *Hub) resultPath(id string) string { return filepath.Join(h.dir, "leases", id+".result.json") }

func (s *session) terminal(l *lease, r Result) {
	if l.timer != nil {
		l.timer.Stop()
	}
	if err := l.add(false, "result", r); err != nil {
		r.Verified = false
		r.Error = errors.Join(errors.New(r.Error), fmt.Errorf("write lease record: %w", err)).Error()
	}
	if err := errors.Join(l.audit.Sync(), l.audit.Close()); err != nil {
		r.Verified = false
		r.Error = errors.Join(errors.New(r.Error), fmt.Errorf("close lease record: %w", err)).Error()
	}
	if err := s.saveResult(r); err != nil {
		r.Verified = false
		r.Error = errors.Join(errors.New(r.Error), fmt.Errorf("save lease result: %w", err)).Error()
	}
	l.result = r
	if l.phase == "offered" {
		l.start <- errors.New(r.Error)
	}
	close(l.done)
	if l.events != nil {
		close(l.events)
	}
	delete(s.active, l.id)
	s.last = l
}

func (s *session) saveResult(r Result) error {
	path := s.hub.resultPath(r.Lease)
	tmp, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := errors.Join(json.NewEncoder(tmp).Encode(savedResult{Donor: s.id, Result: r}), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *session) receive(m wire.Message) error {
	bad := errors.New("unexpected, malformed or out-of-lease message")
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Type == "heartbeat" && m.Lease == "" {
		var status wire.Status
		if m.Decode(&status) != nil || !wire.Digest(status.Image) || len(status.Health.GPUs) > wire.MaxGPUs || (status.FreeGPUs != nil &&
			(*status.FreeGPUs < 0 || *status.FreeGPUs > len(status.Health.GPUs))) ||
			status.Scratch <= 0 || status.Scratch > wire.MaxScratch || status.Health.At.IsZero() || status.Health.At.After(s.hub.now().Add(wire.Silence)) {
			return bad
		}
		s.status = status
		s.conn.Extend()
		return nil
	}
	l := s.active[m.Lease]
	if l == nil {
		return bad
	}
	switch m.Type {
	case "started":
		var started wire.Started
		if l.phase != "offered" || m.Decode(&started) != nil || !wire.Digest(started.Bundle) || started.Health.At.Before(l.offer.Issued.Add(-wire.Silence)) ||
			started.Health.At.After(s.hub.now().Add(wire.Silence)) ||
			(l.offer.Manifest.Bundle != "" && started.Bundle != l.offer.Manifest.Bundle) ||
			len(started.Health.Reasons) != 0 || started.Health.At.IsZero() || !s.validDevices(l, started.Devices) {
			return bad
		}
		if err := l.add(false, m.Type, started); err != nil {
			return err
		}
		l.devices = started.Devices
		l.phase = "ready"
		l.start <- nil
		return nil
	case "refused":
		var reason wire.Reason
		if l.phase != "offered" || m.Decode(&reason) != nil || len(reason.Reason) > 1024 || reason.Reason == "" {
			return bad
		}
		if err := l.add(false, m.Type, reason); err != nil {
			return err
		}
		s.terminal(l, Result{Lease: l.id, Error: reason.Reason})
		return nil
	case "output", "salvage":
		var c wire.Chunk
		if m.Decode(&c) != nil || len(c.Data) == 0 || len(c.Data) > wire.ChunkCap {
			return bad
		}
		if m.Type == "salvage" {
			if l.ending == "" || l.events != nil || c.Stream != "salvage" || c.Number != 0 ||
				l.salvage.Len()+len(c.Data) > wire.SalvageCap {
				return bad
			}
			l.phase = "salvaging"
			l.salvage.Write(c.Data)
		} else {
			if l.events == nil || (c.Stream != "stdout" && c.Stream != "stderr") ||
				c.Number != l.number || l.output+len(c.Data) > wire.OutputCap {
				return bad
			}
			l.output += len(c.Data)
			select {
			case l.events <- Event{Chunk: &c}:
			default:
				return errors.New("command consumer is too slow")
			}
		}
		return l.add(false, m.Type, c)
	case "exit":
		var e wire.Exit
		if l.events == nil || m.Decode(&e) != nil || e.Number != l.number || len(e.Error) > 1024 {
			return bad
		}
		if err := l.add(false, m.Type, e); err != nil {
			return err
		}
		l.exits = append(l.exits, e.Fact)
		select {
		case l.events <- Event{Exit: &e}:
		default:
			return errors.New("command consumer is too slow")
		}
		close(l.events)
		l.events = nil
		if l.phase == "executing" {
			l.phase = "ready"
		}
		return nil
	case "ending":
		var reason wire.Reason
		if l.phase == "offered" || l.ending != "" || m.Decode(&reason) != nil {
			return bad
		}
		switch reason.Reason {
		case "complete", "cancel", "deadline", "health", "lost", "output-limit":
		default:
			return bad
		}
		l.ending, l.phase = reason.Reason, "ending"
		if err := l.add(false, m.Type, reason); err != nil {
			return err
		}
		if err := l.add(true, "seal", struct{}{}); err != nil {
			return err
		}
		return s.conn.Send("seal", l.id, struct{}{})
	case "done":
		var done wire.Done
		if l.phase == "offered" || l.events != nil || m.Decode(&done) != nil ||
			!l.verifyTranscript(s.key, s.id, done, s.hub.now()) {
			return errors.New("transcript refused")
		}
		result := Result{Lease: l.id, Verified: true, Facts: done.Facts, Salvage: l.salvage.Bytes()}
		facts := done.Facts
		if facts.End == "lost" && !facts.Ready && facts.RunnerEnd == "" && facts.SalvageError != "" &&
			len(facts.Commands) == 0 && len(result.Salvage) == 0 {
			// The signature covers this lease's loss, but runner exit evidence is unavailable.
			result.Verified, result.Error = false, "runner result unavailable"
		} else if !l.verifyFacts(facts) {
			return errors.New("transcript refused")
		}
		if err := l.add(false, m.Type, done); err != nil {
			return err
		}

		s.terminal(l, result)
		return nil
	default:
		return bad
	}
}

func (l *lease) verifyTranscript(key ed25519.PublicKey, donor string, d wire.Done, now time.Time) bool {
	return !now.After(l.offer.Deadline.Add(30*time.Second)) && !d.Facts.Health.At.Before(l.offer.Issued.Add(-wire.Silence)) &&
		!d.Facts.Health.At.After(now.Add(wire.Silence)) && len(d.Facts.Commands) <= l.number &&
		len(l.exits) == l.number && slices.Equal(l.devices, d.Facts.Devices) && d.Facts.End == l.ending && l.record.Verify(key, donor, l.id, d)
}

func (l *lease) verifyFacts(facts wire.Facts) bool {
	switch facts.End {
	case "complete":
		if l.requested != "finish" {
			return false
		}
		if facts.RunnerEnd != "complete" || len(facts.Commands) != l.number {
			return false
		}
	case "cancel":
		if l.requested != "cancel" {
			return false
		}
	case "health":
		if len(facts.Health.Reasons) == 0 {
			return false
		}
	case "deadline", "lost", "output-limit":
	default:
		return false
	}
	for i, observed := range l.exits {
		if observed.ExitCode == nil {
			continue
		}
		if i >= len(facts.Commands) || facts.Commands[i].ExitCode == nil ||
			*facts.Commands[i].ExitCode != *observed.ExitCode {
			return false
		}
	}
	return true
}

func (s *session) validDevices(l *lease, devices []string) bool {
	if s.status.FreeGPUs == nil && l.offer.Manifest.GPUs == 0 && len(devices) == 0 {
		return true
	}
	if len(devices) != wire.GPUCount(l.offer.Manifest.GPUs) {
		return false
	}
	known := map[string]bool{}
	for _, gpu := range s.status.Health.GPUs {
		if gpu.UUID != "" {
			known[gpu.UUID] = true
		}
	}
	for _, other := range s.active {
		if other.ending != "" {
			continue
		}
		for _, device := range other.devices {
			delete(known, device)
		}
	}
	for _, device := range devices {
		if !known[device] {
			return false
		}
		delete(known, device)
	}
	return true
}

func (s *session) begin(manifest wire.Manifest, duration time.Duration, scratch int64) (*lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return nil, errors.New("donor disconnected")
	default:
	}
	status := s.status
	image := s.hub.images[status.Arch]
	total, free := 1, 1
	if status.FreeGPUs != nil {
		total, free = len(status.Health.GPUs), *status.FreeGPUs
	}
	reserved := 0
	for _, active := range s.active {
		reserved += wire.GPUCount(active.offer.Manifest.GPUs)
	}
	count := wire.GPUCount(manifest.GPUs)
	if !status.Ready || status.Paused || len(status.Health.Reasons) > 0 ||
		s.hub.now().Sub(status.Health.At) > wire.Silence || image == "" || status.Image != image ||
		count > min(free, total-reserved) || scratch > status.Scratch {
		return nil, fmt.Errorf("donor unavailable (ready=%t paused=%t health=%q)", status.Ready, status.Paused, status.Health.Reasons)
	}
	if duration <= 0 || duration > wire.MaxLease {
		return nil, errors.New("invalid lease duration")
	}
	now := s.hub.now().UTC()
	o := wire.Offer{Nonce: wire.ID(), Image: image, Manifest: manifest, ManifestDigest: wire.ManifestDigest(manifest),
		Deadline: now.Add(duration), Issued: now, Scratch: scratch}
	if manifest.Source != nil {
		o.Head = manifest.Source.Commit
	}
	if !o.Valid(now) {
		return nil, errors.New("invalid manifest or lease")
	}
	id := wire.ID()
	audit, err := os.OpenFile(filepath.Join(s.hub.dir, "leases", id+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	l := &lease{id: id, offer: o, record: wire.NewRecord(), audit: audit, phase: "offered", start: make(chan error, 1), done: make(chan struct{})}
	if s.active == nil {
		s.active = map[string]*lease{}
	}
	s.active[id] = l
	if err := l.add(true, "offer", o); err != nil {
		s.terminal(l, Result{Lease: id, Error: err.Error()})
		return nil, err
	}
	if err := s.conn.Send("offer", id, o); err != nil {
		s.terminal(l, Result{Lease: id, Error: err.Error()})
		s.conn.Close()
		return nil, err
	}
	l.timer = time.AfterFunc(duration+30*time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.active[id] == l {
			s.conn.Close()
		}
	})
	return l, nil
}

func (s *session) command(id string, argv []string) (<-chan Event, *lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.active[id]
	if l == nil || l.phase != "ready" || !s.hub.now().Before(l.offer.Deadline) {
		return nil, nil, errors.New("lease cannot accept a command")
	}
	c := wire.Command{Number: l.number + 1, Argv: argv}
	if !c.Valid() {
		return nil, nil, errors.New("invalid command or command cap reached")
	}
	if err := l.add(true, "exec", c); err != nil {
		s.conn.Close()
		return nil, nil, err
	}
	if err := s.conn.Send("exec", id, c); err != nil {
		s.conn.Close()
		return nil, nil, err
	}
	l.number++
	l.phase, l.events = "executing", make(chan Event, 64)
	return l.events, l, nil
}

func (s *session) end(id, op string) (*lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != nil && s.last.id == id {
		return s.last, nil
	}
	l := s.active[id]
	if l == nil || (op == "finish" && l.phase != "ready") ||
		(op == "cancel" && l.phase != "ready" && l.phase != "executing") {
		return nil, errors.New("lease cannot accept end operation")
	}
	if err := l.add(true, op, struct{}{}); err != nil {
		s.conn.Close()
		return nil, err
	}
	if err := s.conn.Send(op, id, struct{}{}); err != nil {
		s.conn.Close()
		return nil, err
	}
	l.requested, l.phase = op, "finishing"
	if op == "cancel" {
		l.phase = "cancelling"
	}
	return l, nil
}
