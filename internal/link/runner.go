package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"autobot-connector/internal/inputs"
	"autobot-connector/internal/wire"
)

type runner struct{ *http.Client }

type runnerLease struct {
	Lease        string             `json:"lease"`
	End          string             `json:"end"`
	Commands     []wire.CommandFact `json:"commands"`
	OOMBefore    *int64             `json:"oom_before"`
	OOMAfter     *int64             `json:"oom_after"`
	OOMKilled    *bool              `json:"oom_killed"`
	Salvage      []byte             `json:"salvage_tar"`
	SalvageError string             `json:"salvage_error"`
}

type runnerStatus struct {
	Status       string
	Active, Last *runnerLease
}

type runnerEvent struct {
	Stream string
	Data   []byte
	Exit   *int
	Signal *int
	Error  string
}

func newRunner(socket string) runner {
	d := net.Dialer{Timeout: time.Second}
	return runner{&http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", socket) },
		DisableKeepAlives: true,
	}}}
}

func (r runner) open(ctx context.Context, method, path string, input io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://runner"+path, input)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		req.ContentLength = size
	}
	resp, err := r.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("runner HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return resp, nil
}

func (r runner) json(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := r.open(ctx, method, path, body, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2*wire.SalvageCap)).Decode(output)
}

func (r runner) begin(ctx context.Context, id string, offer wire.Offer) error {
	return r.json(ctx, "POST", "/run", struct {
		Lease    string
		Deadline time.Time
		Salvage  []string
		Bundle   bool
	}{id, offer.Deadline, offer.Manifest.Salvage, true}, nil)
}

func (r runner) load(ctx context.Context, id string, b *inputs.Bundle) error {
	q := url.Values{"lease": {id}, "digest": {b.Digest}}
	resp, err := r.open(ctx, "POST", "/load?"+q.Encode(), b.File, b.Size)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (r runner) stop(id, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return r.json(ctx, "POST", "/stop", struct{ Lease, End string }{id, reason}, nil)
}

func (r runner) end(id, reason string, until time.Time) (runnerStatus, error) {
	var s runnerStatus
	var err error
	for time.Now().Before(until) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = r.json(ctx, "GET", "/status", nil, &s)
		cancel()
		if err == nil {
			if s.Last != nil && s.Last.Lease == id {
				return s, nil
			}
			if s.Active != nil && s.Active.Lease == id {
				r.stop(id, reason)
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return runnerStatus{}, errors.Join(errors.New("runner result unavailable"), err)
}
