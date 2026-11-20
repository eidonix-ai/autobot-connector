package wire

import "testing"

func TestManifestGPUCount(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3, 4} {
		if !(Manifest{GPUs: count}).Valid() || GPUCount(count) != max(1, count) {
			t.Fatalf("bad GPU count handling for %d", count)
		}
	}
	for _, count := range []int{-1, 5} {
		if (Manifest{GPUs: count}).Valid() {
			t.Fatalf("accepted invalid GPU count %d", count)
		}
	}
}

func TestWorkLimits(t *testing.T) {
	for profile, want := range map[string]Limits{
		"":      {CPU: 1, PIDs: 48, Memory: 512 << 20, Scratch: ScratchCap, ScratchInodes: 8192},
		"flare": {CPU: 4, PIDs: 256, Memory: 24 << 30, Scratch: MaxScratch, ScratchInodes: 524288},
	} {
		if got, err := WorkLimits(profile); got != want || err != nil {
			t.Fatalf("profile %q = %+v, %v", profile, got, err)
		}
	}
	if _, err := WorkLimits("other"); err == nil {
		t.Fatal("accepted unknown profile")
	}
}
