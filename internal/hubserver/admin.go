package hubserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"autobot-connector/internal/wire"
)

type Donor struct {
	ID        string      `json:"id"`
	Revoked   bool        `json:"revoked"`
	Connected bool        `json:"connected"`
	Status    wire.Status `json:"status"`
}

type Request struct {
	Donor    string        `json:"donor"`
	Lease    string        `json:"lease,omitempty"`
	Manifest wire.Manifest `json:"manifest"`
	Duration time.Duration `json:"duration,omitempty"`
	Scratch  int64         `json:"scratch,omitempty"`
	Argv     []string      `json:"argv,omitempty"`
}

type Begun struct {
	Lease string `json:"lease"`
}
type Code struct {
	Code string `json:"code"`
}

func (h *Hub) donors() []Donor {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := make([]Donor, 0, len(h.state.Donors))
	for id, d := range h.state.Donors {
		entry := Donor{ID: id, Revoked: d.Revoked}
		if s := h.sessions[id]; s != nil {
			s.mu.Lock()
			select {
			case <-s.closed:
			default:
				entry.Connected, entry.Status = !d.Revoked, s.status
			}
			s.mu.Unlock()
		}
		list = append(list, entry)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

func read(w http.ResponseWriter, r *http.Request) (Request, error) {
	var q Request
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.FrameCap))
	if err != nil || wire.Strict(raw, &q) != nil || !wire.Hex(q.Donor, 32) ||
		(q.Lease != "" && !wire.Hex(q.Lease, 32)) {
		return q, errors.New("invalid hub request")
	}
	return q, nil
}

func send(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *Hub) Admin() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /donors", func(w http.ResponseWriter, _ *http.Request) { send(w, h.donors()) })
	mux.HandleFunc("POST /code", func(w http.ResponseWriter, _ *http.Request) {
		code, err := h.code()
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		send(w, Code{code})
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		q, err := read(w, r)
		if err == nil {
			err = h.revoke(q.Donor)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /{op}", func(w http.ResponseWriter, r *http.Request) {
		if err := h.operate(w, r); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
		}
	})
	return mux
}

func (h *Hub) operate(w http.ResponseWriter, r *http.Request) error {
	q, err := read(w, r)
	if err != nil {
		return err
	}
	h.mu.Lock()
	s := h.sessions[q.Donor]
	h.mu.Unlock()
	op := r.PathValue("op")
	if s == nil {
		if op == "result" || op == "finish" || op == "cancel" {
			return h.saved(w, q)
		}
		return errors.New("donor disconnected")
	}
	switch op {
	case "begin":
		l, err := s.begin(q.Manifest, q.Duration, q.Scratch)
		if err != nil {
			return err
		}
		select {
		case err := <-l.start:
			if err != nil {
				return err
			}
			send(w, Begun{l.id})
		case <-r.Context().Done():
			s.disconnect("lease request cancelled")
		}
	case "exec":
		events, l, err := s.command(q.Lease, q.Argv)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		control := http.NewResponseController(w)
		write := func(event Event) bool {
			control.SetWriteDeadline(time.Now().Add(2 * time.Second))
			return json.NewEncoder(w).Encode(event) == nil && control.Flush() == nil
		}
		caller := r.Context().Done()
		delivering := true
		for {
			select {
			case <-caller:
				caller, delivering = nil, false
			case event, ok := <-events:
				if !ok {
					select {
					case <-l.done:
						if delivering && l.result.Error != "" {
							write(Event{Error: l.result.Error})
						}
					default:
					}
					return nil
				}
				if delivering && !write(event) {
					delivering = false
				}
			}
		}
	case "finish", "cancel", "result":
		var l *lease
		if op == "result" {
			s.mu.Lock()
			l = s.active[q.Lease]
			if s.last != nil && s.last.id == q.Lease {
				l = s.last
			}
			s.mu.Unlock()
			if l == nil {
				return h.saved(w, q)
			}
		} else {
			l, err = s.end(q.Lease, op)
			if err != nil {
				return err
			}
		}
		select {
		case <-l.done:
			send(w, l.result)
		case <-r.Context().Done():
		}
	default:
		return errors.New("unknown hub operation")
	}
	return nil
}

func (h *Hub) saved(w http.ResponseWriter, q Request) error {
	if !wire.Hex(q.Lease, 32) {
		return errors.New("lease not found")
	}
	f, err := os.Open(h.resultPath(q.Lease))
	if err != nil {
		return errors.New("lease not found")
	}
	defer f.Close()
	var saved savedResult
	if err := json.NewDecoder(io.LimitReader(f, 2*wire.SalvageCap)).Decode(&saved); err != nil || saved.Donor != q.Donor {
		return errors.New("lease not found")
	}
	send(w, saved.Result)
	return nil
}
