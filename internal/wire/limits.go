package wire

import "fmt"

const MaxGPUs = 4

func GPUCount(count int) int {
	if count == 0 {
		return 1
	}
	return count
}

func ValidGPUCount(count int) bool { return count >= 0 && count <= MaxGPUs }

type Limits struct {
	CPU, PIDs     int
	Memory        int64
	Scratch       int64
	ScratchInodes int
}

func WorkLimits(profile string) (Limits, error) {
	switch profile {
	case "":
		return Limits{CPU: 1, PIDs: 48, Memory: 512 << 20, Scratch: 128 << 20, ScratchInodes: 8192}, nil
	case "flare":
		return Limits{CPU: 4, PIDs: 256, Memory: 24 << 30, Scratch: 16 << 30, ScratchInodes: 524288}, nil
	default:
		return Limits{}, fmt.Errorf("unknown work profile %q", profile)
	}
}
