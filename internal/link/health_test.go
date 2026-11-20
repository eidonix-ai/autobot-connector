package link

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"autobot-connector/internal/health"
	"autobot-connector/internal/wire"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func goodReading() wire.Health {
	temperature, fan, power, clock := 40, 20, 6500, 208
	var throttle, ecc, xid uint64
	return wire.Health{TotalKiB: 1000, AvailableKiB: 600, GPUs: []wire.GPU{{
		Name: "NVIDIA GB10", SM: "12.1", Temperature: &temperature, Fan: &fan,
		PowerMW: &power, GraphicsMHz: &clock, Throttle: &throttle, ECC: &ecc, XIDCount: &xid,
	}}}
}

func TestHealthStopsJobsAndRecovers(t *testing.T) {
	tests := []struct {
		name, reason string
		bad          func(*wire.Health)
		manual       bool
	}{
		{"temperature", "GPU temperature", func(h *wire.Health) { *h.GPUs[0].Temperature = 83 }, false},
		{"throttling", "GPU throttling", func(h *wire.Health) { *h.GPUs[0].Throttle = 0x40 }, false},
		{"fan", "GPU fan requires donor clear", func(h *wire.Health) { *h.GPUs[0].Temperature = 61; *h.GPUs[0].Fan = 0 }, true},
		{"ECC", "GPU ECC or Xid requires donor clear", func(h *wire.Health) { *h.GPUs[0].ECC = 1 }, true},
		{"Xid", "GPU ECC or Xid requires donor clear", func(h *wire.Health) { *h.GPUs[0].XIDCount = 1 }, true},
		{"host-memory", "host memory", func(h *wire.Health) { h.AvailableKiB = 99 }, false},
		{"duty-cycle", "GPU duty cycle", func(*wire.Health) {}, false},
		{"missing-mandatory-reading", "mandatory GB10 readings unavailable", func(h *wire.Health) { h.GPUs[0].Temperature = nil }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			calls := make(chan string, 1)
			l := Link{config: Config{State: t.TempDir(), Image: "sha256:" + strings.Repeat("0", 64)}}
			l.runner = runner{&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/readiness" {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"Status":"ready"}`))}, nil
				}
				b, _ := io.ReadAll(r.Body)
				var req struct{ Lease, End string }
				json.Unmarshal(b, &req)
				calls <- r.Method + " " + r.URL.Path + " " + req.Lease + " " + req.End
				return &http.Response{StatusCode: 202, Body: http.NoBody}, nil
			})}}
			id := "test-lease"
			var verify func()
			if socket := os.Getenv("DONOR_TEST_RUNNER_SOCKET"); socket != "" {
				l.runner = newRunner(socket)
				id, verify = liveHealthJob(t, l.runner)
			}
			j := &job{session: session{Link: &l}, id: id, ctx: ctx, cancel: cancel}
			j.working.Store(true)
			l.state.job = j
			base := time.Now().UTC()
			feed := func(offset time.Duration, clear bool, modify func(*wire.Health)) {
				h := goodReading()
				h.At = base.Add(offset)
				if modify != nil {
					modify(&h)
				}
				l.applySample(h, false, clear)
			}
			feed(0, false, nil)
			at := time.Second
			if tt.name == "duty-cycle" {
				at = health.Duty
			}
			feed(at, false, tt.bad)
			if got := l.latest().Reasons; len(got) != 1 || got[0] != tt.reason {
				t.Fatalf("trigger: %v", got)
			}
			if verify != nil {
				verify()
			} else {
				select {
				case request := <-calls:
					if request != "POST /stop test-lease cancel" {
						t.Fatal(request)
					}
					t.Log(request, "for", tt.reason)
				case <-time.After(time.Second):
					t.Fatal("health trigger did not send runner stop")
				}
			}
			if j.interrupted() != "health" {
				t.Fatal("job was not interrupted by health")
			}
			j.release()
			recover := at + time.Second
			if tt.name == "host-memory" {
				at20 := func(h *wire.Health) { h.AvailableKiB = 200 }
				feed(recover, false, at20)
				feed(recover+health.Recovery, false, at20)
				if len(l.latest().Reasons) == 0 {
					t.Fatal("20% is below the strict recovery condition")
				}
				recover += health.Recovery + time.Second
			}
			feed(recover, false, nil)
			switch {
			case tt.manual:
				feed(recover+health.Recovery, false, nil)
				if len(l.latest().Reasons) == 0 {
					t.Fatal("manual latch cleared without donor")
				}
				feed(recover+health.Recovery, true, nil)
				t.Log("stayed latched until donor clear")
			case tt.name == "missing-mandatory-reading":
				t.Log("mandatory reading restored")
			default:
				wait := health.Recovery
				if tt.name == "duty-cycle" {
					wait = health.Rest
				}
				feed(recover+wait-time.Nanosecond, false, nil)
				if len(l.latest().Reasons) == 0 {
					t.Fatal("recovered too early")
				}
				feed(recover+wait, false, nil)
				t.Logf("recovered after %s of healthy readings or rest", wait)
			}
			if status := l.status(context.Background()); !status.Ready {
				t.Fatalf("not ready after recovery: %+v", status)
			}
		})
	}
}

// The standalone acceptance run uses real job containers and an injected clock.
func liveHealthJob(t *testing.T, r runner) (string, func()) {
	t.Helper()
	id := wire.ID()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() { r.stop(id, "cancel") })
	var start struct{ Container string }
	if err := r.json(ctx, "POST", "/run", struct {
		Lease    string
		Deadline time.Time
	}{id, time.Now().Add(20 * time.Second)}, &start); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		Lease string
		Argv  []string
	}{id, []string{"/bin/sh", "-c", "echo live-health-job; exec /bin/sleep 60"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.open(ctx, "POST", "/exec", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	decoder := json.NewDecoder(resp.Body)
	var first runnerEvent
	if err := decoder.Decode(&first); err != nil || string(first.Data) != "live-health-job\n" {
		t.Fatalf("workload never started: %+v %v", first, err)
	}
	return id, func() {
		t.Helper()
		var exit runnerEvent
		if err := decoder.Decode(&exit); err != nil || exit.Exit == nil || *exit.Exit != 137 {
			t.Fatalf("uncancelled workload: %+v %v", exit, err)
		}
		for until := time.Now().Add(4 * time.Second); time.Now().Before(until); time.Sleep(100 * time.Millisecond) {
			var status runnerStatus
			if err := r.json(ctx, "GET", "/status", nil, &status); err == nil &&
				status.Status == "ready" && status.Active == nil && status.Last != nil &&
				status.Last.Lease == id && status.Last.End == "cancel" {
				t.Logf("live workload %s started, exited %d, container removed, runner ready", id, *exit.Exit)
				return
			}
		}
		t.Fatal("runner did not confirm cancellation and teardown")
	}
}
