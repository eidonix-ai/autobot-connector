package main

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestFreeGPUs(t *testing.T) {
	var spec cdiSpec
	if err := json.Unmarshal([]byte(`{"devices":[{"name":"GPU-a"},{"name":"GPU-b"},{"name":"GPU-c"},{"name":"GPU-d"},{"name":"GPU-e"},{"name":"all"}]}`), &spec); err != nil {
		t.Fatal(err)
	}
	memory := map[string]int{"GPU-a": 2, "GPU-b": 2, "GPU-c": 66, "GPU-d": 67, "GPU-e": 2}
	baseline := map[string]int{"GPU-c": 2, "GPU-d": 2}
	free, err := spec.freeGPUs(map[string]bool{"GPU-a": true}, map[string]bool{"GPU-b": true}, memory, baseline)
	if err != nil || !slices.Equal(free, []string{"GPU-c"}) {
		t.Fatalf("free GPUs = %v, %v", free, err)
	}
	if got := deviceRequests(free); !slices.Equal(got, []string{"flare.local/gpu=GPU-c"}) {
		t.Fatal(got)
	}
	if _, err = spec.freeGPUs(nil, nil, map[string]int{}, nil); err == nil {
		t.Fatal("missing discrete memory was accepted")
	}
	free, err = spec.freeGPUs(nil, nil, nil, baseline)
	if err != nil || len(free) != 5 {
		t.Fatalf("GPU-only selection = %v, %v", free, err)
	}
}

func TestLeaseResultWaitsForGPURecovery(t *testing.T) {
	j := &lease{leaseRequest: leaseRequest{Lease: "job"}, Devices: []string{"GPU-a", "GPU-b"}}
	r := runner{state: state{Leases: map[string]*lease{j.Lease: j}}, results: map[string]*lease{}, cleaned: map[string]bool{}}
	r.complete(j)
	if err := r.verifyRecovery([]string{"GPU-a", "GPU-c"}, nil); err != nil {
		t.Fatal(err)
	}
	if s := r.snapshot(j.Lease); s.Active != nil || s.Last != nil {
		t.Fatal("unreleased allocation appeared as complete")
	}
	if err := r.verifyRecovery([]string{"GPU-a", "GPU-b"}, nil); err != nil {
		t.Fatal(err)
	}
	if s := r.snapshot(j.Lease); s.Active != nil || s.Last != j {
		t.Fatal("released allocation lost its result")
	}
}

func TestGB10ResultWaitsForHostMemory(t *testing.T) {
	previous := gpuModel
	gpuModel = "gb10"
	t.Cleanup(func() { gpuModel = previous })
	r := runner{state: state{Baseline: 2 * hostMemoryAllowanceKiB}, results: map[string]*lease{}, cleaned: map[string]bool{}}
	j := &lease{leaseRequest: leaseRequest{Lease: "job"}, Devices: []string{"GPU-a"}}
	r.complete(j)
	mem := int64(hostMemoryAllowanceKiB - 1)
	read := func() (int64, error) { return mem, nil }
	if err := r.verifyRecovery(j.Devices, read); err == nil {
		t.Fatal("memory below the GB10 recovery floor was accepted")
	}
	if r.snapshot(j.Lease).Last != nil || r.snapshot("").Last != nil {
		t.Fatal("result was visible below the recovery floor")
	}
	mem++
	if err := r.verifyRecovery(j.Devices, read); err != nil {
		t.Fatal(err)
	}
	if r.snapshot(j.Lease).Last != j || r.snapshot("").Last != j {
		t.Fatal("result was not visible at the recovery floor")
	}
}

func TestVRAMBaselineStaysFixedAfterUse(t *testing.T) {
	r := runner{state: state{Leases: map[string]*lease{}}, vram: map[string]int{}, usedGPUs: map[string]bool{}}
	r.updateVRAM(map[string]bool{"GPU-b": true}, map[string]int{"GPU-a": 2, "GPU-b": 8})
	if _, ok := r.vram["GPU-b"]; ok {
		t.Fatal("recorded a baseline for a busy GPU")
	}
	r.updateVRAM(nil, map[string]int{"GPU-a": 3})
	if r.vram["GPU-a"] != 3 {
		t.Fatal("idle GPU baseline did not refresh before its first lease")
	}
	r.usedGPUs["GPU-a"] = true
	r.Leases["job"] = &lease{}
	r.updateVRAM(nil, map[string]int{"GPU-b": 9})
	if _, ok := r.vram["GPU-b"]; ok {
		t.Fatal("recorded a first baseline while another lease was active")
	}
	delete(r.Leases, "job")
	r.updateVRAM(nil, map[string]int{"GPU-a": 45, "GPU-b": 9})
	r.updateVRAM(nil, map[string]int{"GPU-a": 68, "GPU-b": 9})
	if r.vram["GPU-a"] != 3 || r.vram["GPU-b"] != 9 {
		t.Fatalf("idle baselines after teardown = %v", r.vram)
	}
	var spec cdiSpec
	if err := json.Unmarshal([]byte(`{"devices":[{"name":"GPU-a"},{"name":"GPU-b"}]}`), &spec); err != nil {
		t.Fatal(err)
	}
	free, err := spec.freeGPUs(nil, nil, map[string]int{"GPU-a": 68, "GPU-b": 9}, r.vram)
	if err != nil || !slices.Equal(free, []string{"GPU-b"}) {
		t.Fatalf("free GPUs after cumulative VRAM growth = %v, %v", free, err)
	}
}

func TestGPUModelMatchesAllNVMLDevices(t *testing.T) {
	const gb10 = "NVIDIA GB10"
	const discrete = "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition"
	if err := checkGPUModel("gb10", gb10); err != nil {
		t.Fatal(err)
	}
	if err := checkGPUModel("rtx-pro-6000", discrete+"\n"+discrete); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ model, names string }{{"", gb10}, {"gb10", ""}, {"gb10", discrete}, {"rtx-pro-6000", discrete + "\n" + gb10}} {
		if err := checkGPUModel(tc.model, tc.names); err == nil {
			t.Fatalf("accepted model %q with NVML names %q", tc.model, tc.names)
		}
	}
}
