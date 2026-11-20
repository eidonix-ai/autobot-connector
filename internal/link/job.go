package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"autobot-connector/internal/inputs"
	"autobot-connector/internal/wire"
)

type job struct {
	session
	id                     string
	offer                  wire.Offer
	ctx                    context.Context
	deadlineCancel         context.CancelFunc
	cancel                 context.CancelCauseFunc
	working                atomic.Bool
	mu                     sync.Mutex
	record                 *wire.Record
	phase                  string
	number, output         int
	commands               chan wire.Command
	finished, sealed       chan struct{}
	stopSent, controlEnded bool
	trigger                wire.Health
}

func (s session) offer(id string, o wire.Offer) error {
	if err := s.remember(o.Issued); err != nil {
		return err
	}
	_, pauseErr := os.Stat(filepath.Join(s.config.State, "pause"))
	s.state.mu.Lock()
	sample := s.state.sample
	if !errors.Is(pauseErr, os.ErrNotExist) || sample.At.IsZero() || time.Since(sample.At) > wire.Silence || len(sample.Reasons) > 0 {
		s.state.mu.Unlock()
		return s.conn.Send("refused", id, wire.Reason{Reason: "donor paused or unhealthy"})
	}
	dc, deadlineCancel := context.WithDeadline(context.Background(), o.Deadline)
	ctx, cancel := context.WithCancelCause(dc)
	j := &job{session: s, id: id, offer: o, ctx: ctx, deadlineCancel: deadlineCancel, cancel: cancel,
		record: wire.NewRecord(), phase: "preparing", commands: make(chan wire.Command, 1), finished: make(chan struct{}), sealed: make(chan struct{})}
	j.record.Add(true, "offer", o)
	s.state.job = j
	s.state.mu.Unlock()
	go j.work()
	return nil
}

func (s session) remember(issued time.Time) error {
	p := filepath.Join(s.config.State, "last-offer")
	if b, err := os.ReadFile(p); err == nil {
		prev, err := time.Parse(time.RFC3339Nano, string(b))
		if err != nil || !issued.After(prev) {
			return errors.New("stale or replayed lease offer")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(p+".tmp", []byte(issued.Format(time.RFC3339Nano)), 0600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (j *job) interrupted() string {
	cause := context.Cause(j.ctx)
	if cause == nil {
		return ""
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return "deadline"
	}
	return cause.Error()
}

func (j *job) interrupt(reason string, sample wire.Health) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.interruptLocked(reason, sample)
}

func (j *job) interruptLocked(reason string, sample wire.Health) {
	if j.ctx.Err() == nil {
		j.trigger = sample
		j.cancel(errors.New(reason))
	}
	j.stopLocked()
}

func (j *job) stopLocked() {
	reason := j.interrupted()
	if j.working.Load() && !j.stopSent && reason != "" && reason != "deadline" {
		j.stopSent = true
		go j.runner.stop(j.id, runnerEnd(reason))
	}
}

func runnerEnd(reason string) string {
	switch reason {
	case "complete":
		return "complete"
	case "deadline":
		return ""
	default:
		return "cancel"
	}
}

func (j *job) send(kind string, value any) error {
	j.record.Add(false, kind, value)
	err := j.conn.Send(kind, j.id, value)
	if err != nil {
		j.conn.Close()
		j.interruptLocked("lost", wire.Health{})
	}
	return err
}

func (j *job) receive(m wire.Message) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.phase == "preparing" || j.phase == "finished" || j.phase == "sealed" {
		return false
	}
	switch m.Type {
	case "exec":
		var c wire.Command
		if (j.phase != "ready" && j.phase != "closing") || j.controlEnded || m.Decode(&c) != nil ||
			!c.Valid() || c.Number != j.number+1 {
			return false
		}
		j.number = c.Number
		j.record.Add(true, m.Type, c)
		if j.phase == "closing" {
			return j.send("exit", wire.Exit{Number: c.Number, Error: "lease is ending"}) == nil
		}
		j.phase = "executing"
		j.commands <- c
		return true
	case "finish", "cancel":
		if j.controlEnded || m.Decode(&struct{}{}) != nil || (m.Type == "finish" && j.phase == "executing") {
			return false
		}
		j.record.Add(true, m.Type, struct{}{})
		j.controlEnded = true
		end := "cancel"
		if m.Type == "finish" {
			end = "complete"
		}
		j.interruptLocked(end, wire.Health{})
		return true
	case "seal":
		if j.phase != "closing" || m.Decode(&struct{}{}) != nil {
			return false
		}
		j.record.Add(true, m.Type, struct{}{})
		j.phase = "sealed"
		close(j.sealed)
		return true
	default:
		return false
	}
}

func (j *job) release() {
	j.working.Store(false)
	j.state.mu.Lock()
	if j.state.job == j {
		j.state.job = nil
	}
	j.state.mu.Unlock()
}

func (j *job) work() {
	defer close(j.finished)
	defer j.deadlineCancel()
	defer j.release()
	b, err := j.prepare()
	if err != nil {
		j.working.Store(false)
		j.mu.Lock()
		j.phase = "finished"
		j.release()
		j.conn.Send("refused", j.id, wire.Reason{Reason: trim(err.Error())})
		j.mu.Unlock()
		return
	}
	j.mu.Lock()
	sample := j.latest()
	if j.ctx.Err() != nil || len(sample.Reasons) > 0 {
		j.interruptLocked("health", sample)
		j.mu.Unlock()
		j.runner.end(j.id, "cancel", time.Now().Add(12*time.Second))
		j.mu.Lock()
		j.phase = "finished"
		j.release()
		j.conn.Send("refused", j.id, wire.Reason{Reason: "donor stopped during preparation"})
		j.mu.Unlock()
		return
	}
	err = j.send("started", wire.Started{Bundle: b, Health: sample})
	j.phase = "ready"
	j.mu.Unlock()
	if err == nil {
		j.execute()
	}
	j.finalize()
}

func (j *job) prepare() (string, error) {
	client := inputs.PublicClient()
	defer client.CloseIdleConnections()
	b, err := inputs.Build(j.ctx, client, j.offer.Manifest, j.offer.Scratch)
	if err != nil {
		return "", err
	}
	defer b.Close()
	if err := j.ctx.Err(); err != nil {
		return "", err
	}
	// A /run request must settle before cancellation can be treated as proof that
	// no container was created.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = j.runner.begin(ctx, j.id, j.offer)
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var status struct{ Active string }
		statusErr := j.runner.json(ctx, "GET", "/readiness", nil, &status)
		cancel()
		if timedOut || statusErr != nil || status.Active == j.id {
			j.runner.end(j.id, "cancel", time.Now().Add(12*time.Second))
		}
		return "", err
	}
	j.working.Store(true)
	j.mu.Lock()
	j.stopLocked()
	j.mu.Unlock()
	if err := j.runner.load(j.ctx, j.id, b); err != nil {
		j.runner.end(j.id, "cancel", time.Now().Add(12*time.Second))
		return "", err
	}
	return b.Digest, nil
}

func (j *job) execute() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-j.ctx.Done():
			return
		case c := <-j.commands:
			j.exec(c)
		case <-tick.C:
			ctx, cancel := context.WithTimeout(j.ctx, time.Second)
			var status struct{ Active, Last, End string }
			err := j.runner.json(ctx, "GET", "/readiness", nil, &status)
			cancel()
			if err == nil && status.Active != j.id && j.ctx.Err() == nil {
				reason := "lost"
				if status.Last == j.id && status.End == "deadline" {
					reason = "deadline"
				}
				j.interrupt(reason, wire.Health{})
			}
		}
	}
}

func (j *job) exec(c wire.Command) {
	exit := wire.Exit{Number: c.Number}
	if j.ctx.Err() == nil {
		exit.Fact, exit.Error = j.stream(c)
	} else {
		exit.Error = "lease is ending"
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.phase = "ready"
	j.send("exit", exit)
}

func (j *job) stream(c wire.Command) (wire.CommandFact, string) {
	var fact wire.CommandFact
	ctx, cancel := context.WithDeadline(context.Background(), j.offer.Deadline.Add(12*time.Second))
	defer cancel()
	data, err := json.Marshal(struct {
		Lease string
		Argv  []string
	}{j.id, c.Argv})
	if err != nil {
		return fact, trim(err.Error())
	}
	resp, err := j.runner.open(ctx, "POST", "/exec", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fact, trim(err.Error())
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 2*wire.OutputCap))
	for {
		var e runnerEvent
		if err := dec.Decode(&e); err != nil {
			j.interrupt("lost", wire.Health{})
			return fact, trim(fmt.Sprint("runner command stream: ", err))
		}
		if e.Exit != nil || e.Error != "" {
			return wire.CommandFact{ExitCode: e.Exit, Signal: e.Signal}, trim(e.Error)
		}
		j.mu.Lock()
		if j.output+len(e.Data) > wire.OutputCap {
			j.interruptLocked("output-limit", wire.Health{})
			j.mu.Unlock()
			return fact, "job exceeded the output cap"
		}
		j.output += len(e.Data)
		err := j.send("output", wire.Chunk{Number: c.Number, Stream: e.Stream, Data: e.Data})
		j.mu.Unlock()
		if err != nil {
			return fact, "hub stream unavailable"
		}
	}
}

func (j *job) finalize() {
	j.mu.Lock()
	select {
	case pending := <-j.commands:
		j.send("exit", wire.Exit{Number: pending.Number, Error: "lease is ending"})
	default:
	}
	reason := j.interrupted()
	if reason == "" {
		reason = "lost"
	}
	j.phase = "closing"
	announced := j.send("ending", wire.Reason{Reason: reason}) == nil
	j.stopLocked()
	j.mu.Unlock()
	state, err := j.runner.end(j.id, runnerEnd(reason), time.Now().Add(15*time.Second))
	j.working.Store(false)
	if !announced {
		return
	}
	select {
	case <-j.sealed:
	case <-time.After(wire.Silence):
		j.conn.Close()
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.trigger.At.IsZero() {
		j.trigger = j.latest()
	}
	facts := wire.Facts{End: reason, Health: j.trigger, Ready: state.Status == "ready"}
	if err != nil {
		facts.SalvageError = trim(err.Error())
	} else {
		last := state.Last
		facts.RunnerEnd, facts.Commands = last.End, last.Commands
		facts.OOMBefore, facts.OOMAfter, facts.OOMKilled = last.OOMBefore, last.OOMAfter, last.OOMKilled
		facts.SalvageError = trim(last.SalvageError)
		for data := last.Salvage; len(data) > 0; {
			n := min(wire.ChunkCap, len(data))
			if j.send("salvage", wire.Chunk{Stream: "salvage", Data: data[:n]}) != nil {
				return
			}
			data = data[n:]
		}
	}
	if facts.End == "complete" && facts.RunnerEnd != "complete" {
		// The hub will reject a completion whose evidence is incomplete.
		log.Printf("runner completion unavailable for lease %s", j.id)
	}
	done := j.record.Sign(j.key, j.donor, j.id, facts)
	j.phase = "finished"
	j.release()
	if err := j.conn.Send("done", j.id, done); err != nil {
		j.conn.Close()
	}
}

func trim(s string) string {
	if len(s) > 512 {
		return s[:512]
	}
	return s
}
