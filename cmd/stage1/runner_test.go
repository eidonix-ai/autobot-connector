package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeDocker func(method, path string) (int, string)

func (f fakeDocker) RoundTrip(req *http.Request) (*http.Response, error) {
	status, body := f(req.Method, strings.TrimPrefix(req.URL.Path, "/v1.55"))
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func testRunner(t *testing.T, docker fakeDocker) *runner {
	return &runner{
		engine:    engine{&http.Client{Transport: docker}},
		statePath: filepath.Join(t.TempDir(), "lease.json"),
		state:     state{Leases: map[string]*lease{}},
		results:   map[string]*lease{},
		cleaned:   map[string]bool{},
		usedGPUs:  map[string]bool{},
	}
}

func TestKeeperFailureKeepsOtherLease(t *testing.T) {
	removals := 0
	r := testRunner(t, func(method, path string) (int, string) {
		switch method + " " + path {
		case "GET /containers/healthy-container/json":
			return http.StatusOK, `{"State":{"Running":true}}`
		case "GET /containers/failed-container/json":
			return http.StatusNotFound, ""
		case "DELETE /containers/failed-container":
			removals++
			if removals == 1 {
				return http.StatusInternalServerError, "remove failed"
			}
			return http.StatusNoContent, ""
		case "GET /containers/json":
			return http.StatusOK, `[{"Id":"healthy-container","Labels":{"org.flare.donor.lease":"healthy"}}]`
		default:
			return http.StatusNotFound, ""
		}
	})
	deadline := time.Now().Add(time.Minute)
	healthy := &lease{leaseRequest: leaseRequest{Lease: "healthy", Deadline: deadline}, Container: "healthy-container", Devices: []string{"GPU-a"}}
	failed := &lease{leaseRequest: leaseRequest{Lease: "failed", Deadline: deadline}, Container: "failed-container", Devices: []string{"GPU-b"}}
	r.Leases[healthy.Lease], r.Leases[failed.Lease] = healthy, failed

	r.tick()
	if r.dirty || r.Leases[failed.Lease] != failed || failed.End != "lost" || removals != 1 {
		t.Fatalf("failed cleanup must retain its own reservation: dirty=%t removals=%d lease=%+v", r.dirty, removals, r.Leases[failed.Lease])
	}
	r.tick()
	if r.dirty || r.Leases[healthy.Lease] != healthy || healthy.End != "" || r.Leases[failed.Lease] != nil || removals != 2 {
		t.Fatalf("cleanup affected the healthy lease or retained the lost lease: dirty=%t removals=%d leases=%+v", r.dirty, removals, r.Leases)
	}
	if err := r.verifyRecovery(failed.Devices, nil); err != nil {
		t.Fatal(err)
	}
	if got := r.snapshot(failed.Lease).Last; got == nil || got.End != "lost" {
		t.Fatalf("lost result after GPU recovery = %+v", got)
	}
}

func TestFailedStartRemovesOnlyItsContainer(t *testing.T) {
	for _, failure := range []string{"create", "start"} {
		t.Run(failure, func(t *testing.T) {
			var deleted []string
			r := testRunner(t, func(method, path string) (int, string) {
				switch method + " " + path {
				case "POST /containers/create":
					if failure == "create" {
						return http.StatusInternalServerError, "container ID unavailable"
					}
					return http.StatusCreated, `{"Id":"failed-container"}`
				case "POST /containers/failed-container/start":
					return http.StatusInternalServerError, "start failed"
				case "GET /containers/json":
					return http.StatusOK, `[{"Id":"healthy-container","Labels":{"org.flare.donor.lease":"healthy"}},{"Id":"failed-container","Labels":{"org.flare.donor.lease":"failed"}}]`
				default:
					if method == "DELETE" {
						deleted = append(deleted, path)
						return http.StatusNoContent, ""
					}
					return http.StatusNotFound, ""
				}
			})
			healthy := &lease{leaseRequest: leaseRequest{Lease: "healthy"}, Container: "healthy-container", Devices: []string{"GPU-a"}}
			r.Leases[healthy.Lease] = healthy
			if _, err := r.startLease(leaseRequest{Lease: "failed"}, cdiSpec{}, []string{"GPU-b"}); err == nil {
				t.Fatal("failed start was accepted")
			}
			if r.dirty || r.Leases[healthy.Lease] != healthy || r.Leases["failed"] != nil || r.results["failed"].End != "lost" ||
				len(deleted) != 1 || deleted[0] != "/containers/failed-container" {
				t.Fatalf("failed start affected other leases: dirty=%t leases=%v removed=%v", r.dirty, r.Leases, deleted)
			}
		})
	}
}

func TestUnavailableDockerTriggersReconcile(t *testing.T) {
	reachable := false
	r := testRunner(t, func(method, path string) (int, string) {
		if !reachable {
			return http.StatusServiceUnavailable, ""
		}
		switch method + " " + path {
		case "GET /info":
			return http.StatusOK, `{"Driver":"overlayfs","CgroupDriver":"systemd","CgroupVersion":"2"}`
		case "GET /containers/json":
			return http.StatusOK, `[]`
		default:
			return http.StatusNotFound, ""
		}
	})
	r.Leases["active"] = &lease{leaseRequest: leaseRequest{Lease: "active"}, Container: "active-container"}
	r.tick()
	if !r.dirty {
		t.Fatal("runner did not enter reconciliation while Docker was unavailable")
	}
	reachable = true
	r.tick()
	data, err := json.Marshal(r.state)
	if err != nil || r.dirty || len(r.Leases) != 0 || r.Last == nil || r.Last.End != "lost" {
		t.Fatalf("runner did not reconcile after Docker recovered: dirty=%t state=%s error=%v", r.dirty, data, err)
	}
}
