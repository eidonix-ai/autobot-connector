package health

import (
	"slices"
	"testing"
	"time"

	"autobot-connector/internal/wire"
)

func reading[T any](value T) *T { return &value }

func discrete(uuid string) wire.GPU {
	return wire.GPU{
		UUID: uuid, Name: "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition", SM: "12.0",
		Temperature: reading(30), PowerMW: reading(15000), PowerLimitMW: reading(300000),
		GraphicsMHz: reading(180), MemoryMHz: reading(405), Fan: reading(30),
		Throttle: reading(uint64(0)), MemoryMiB: reading(97887), MemoryUsedMiB: reading(2),
	}
}

func TestDiscreteReadings(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*wire.GPU){
		"unknown model": func(g *wire.GPU) { g.Name = "other" },
		"wrong SM":      func(g *wire.GPU) { g.SM = "12.1" },
		"UUID":          func(g *wire.GPU) { g.UUID = "first" },
		"temperature":   func(g *wire.GPU) { g.Temperature = nil },
		"power":         func(g *wire.GPU) { g.PowerMW = nil },
		"power limit":   func(g *wire.GPU) { g.PowerLimitMW = nil },
		"graphics":      func(g *wire.GPU) { g.GraphicsMHz = nil },
		"memory clock":  func(g *wire.GPU) { g.MemoryMHz = nil },
		"fan":           func(g *wire.GPU) { g.Fan = nil },
		"clock events":  func(g *wire.GPU) { g.Throttle = nil },
		"memory total":  func(g *wire.GPU) { g.MemoryMiB = nil },
		"memory used":   func(g *wire.GPU) { g.MemoryUsedMiB = nil },
		"invalid used":  func(g *wire.GPU) { g.MemoryUsedMiB = reading(100000) },
	} {
		t.Run(name, func(t *testing.T) {
			tracker := Tracker{}
			sample := wire.Health{At: now, TotalKiB: 1000, AvailableKiB: 90, GPUs: []wire.GPU{discrete("first"), discrete("second")}}
			if got := tracker.Read(sample, true, false).Reasons; len(got) != 0 {
				t.Fatalf("discrete cards refused with low host memory or optional readings absent: %v", got)
			}
			mutate(&sample.GPUs[1])
			if got := tracker.Read(sample, true, false).Reasons; !slices.Equal(got, []string{"mandatory GPU readings unavailable"}) {
				t.Fatal(got)
			}
		})
	}
}

func TestErrorCountersFollowGPUIdentity(t *testing.T) {
	first, second := discrete("first"), discrete("second")
	first.ECC, second.ECC = reading(uint64(1)), reading(uint64(2))
	sample := wire.Health{At: time.Now(), GPUs: []wire.GPU{first, second}}
	tracker := Tracker{}
	tracker.Read(sample, true, false)
	sample.GPUs = []wire.GPU{second, first}
	if got := tracker.Read(sample, true, false).Reasons; len(got) != 0 {
		t.Fatal(got)
	}
	sample.GPUs[1].ECC = reading(uint64(2))
	if got := tracker.Read(sample, true, false).Reasons; !slices.Equal(got, []string{"GPU ECC or Xid requires donor clear"}) {
		t.Fatal(got)
	}
}
