package health

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"autobot-connector/internal/wire"
)

const (
	Interval = 250 * time.Millisecond
	Recovery = 5 * time.Minute
	Duty     = 2 * time.Hour
	Rest     = 15 * time.Minute
	// Hardware slowdown, software thermal, hardware thermal, hardware power brake.
	ThrottleMask = 0x68 | 0x80
)

type rule struct {
	on        bool
	goodSince time.Time
}

func (r *rule) update(now time.Time, trigger, recovered bool) {
	switch {
	case trigger:
		r.on, r.goodSince = true, time.Time{}
	case !recovered:
		r.goodSince = time.Time{}
	case r.on && r.goodSince.IsZero():
		r.goodSince = now
	case r.on && now.Sub(r.goodSince) >= Recovery:
		*r = rule{}
	}
}

type Checkpoint struct {
	Temperature, Throttle, Memory, Fan, Errors bool
	Work                                       time.Duration
	WorkingSince, IdleSince                    time.Time
}

type Tracker struct {
	MaxTemperature                int
	temperature, throttle, memory rule
	fan, errors                   bool
	previous                      []wire.GPU
	work                          time.Duration
	workingSince, idleSince       time.Time
	duty                          bool
}

func (t *Tracker) Restore(s Checkpoint) {
	t.temperature.on, t.throttle.on, t.memory.on = s.Temperature, s.Throttle, s.Memory
	t.fan, t.errors, t.work = s.Fan, s.Errors, s.Work
	t.workingSince, t.idleSince = s.WorkingSince, s.IdleSince
}

func (t *Tracker) Checkpoint() Checkpoint {
	return Checkpoint{t.temperature.on, t.throttle.on, t.memory.on, t.fan, t.errors,
		t.work, t.workingSince, t.idleSince}
}

func (t *Tracker) Read(sample wire.Health, working, clear bool) wire.Health {
	now := sample.At
	if t.MaxTemperature == 0 {
		t.MaxTemperature = 83
	}
	var reasons []string
	if clear {
		t.fan, t.errors = false, false
	}
	memoryAvailable := sample.TotalKiB > 0 && sample.AvailableKiB >= 0 && sample.AvailableKiB <= sample.TotalKiB
	t.memory.update(now,
		memoryAvailable && sample.AvailableKiB*10 < sample.TotalKiB,
		memoryAvailable && sample.AvailableKiB*5 > sample.TotalKiB)
	if !memoryAvailable {
		reasons = append(reasons, "host memory reading unavailable")
	}
	if t.memory.on {
		reasons = append(reasons, "host memory")
	}

	valid := len(sample.GPUs) == 1
	hot, throttled, cool := false, false, true
	for i, gpu := range sample.GPUs {
		// GB10 requires temperature, power draw, graphics clocks and event reasons.
		// Power limit, memory clocks, memory capacity, fan, ECC and Xid vary by driver.
		valid = valid && gpu.Name == "NVIDIA GB10" && gpu.SM == "12.1" &&
			gpu.Temperature != nil && gpu.PowerMW != nil && gpu.GraphicsMHz != nil && gpu.Throttle != nil
		if gpu.Temperature != nil {
			hot = hot || *gpu.Temperature >= t.MaxTemperature
			cool = cool && *gpu.Temperature <= t.MaxTemperature-10
			t.fan = t.fan || (gpu.Fan != nil && *gpu.Fan == 0 && *gpu.Temperature > 60)
		}
		if gpu.Throttle != nil {
			throttled = throttled || (*gpu.Throttle&ThrottleMask != 0)
		}
		if gpu.XIDCount != nil && *gpu.XIDCount != 0 &&
			(i >= len(t.previous) || t.previous[i].XIDCount == nil || *t.previous[i].XIDCount != *gpu.XIDCount) {
			t.errors = true
		}
		if i < len(t.previous) && rose(t.previous[i].ECC, gpu.ECC) {
			t.errors = true
		}
	}
	if len(sample.GPUs) != 0 {
		t.previous = sample.GPUs
	}
	if !valid {
		reasons = append(reasons, "mandatory GB10 readings unavailable")
	}
	t.temperature.update(now, hot, valid && cool)
	t.throttle.update(now, throttled, valid && !throttled)
	if t.temperature.on {
		reasons = append(reasons, "GPU temperature")
	}
	if t.throttle.on {
		reasons = append(reasons, "GPU throttling")
	}
	if t.fan {
		reasons = append(reasons, "GPU fan requires donor clear")
	}
	if t.errors {
		reasons = append(reasons, "GPU ECC or Xid requires donor clear")
	}

	if working {
		if !t.idleSince.IsZero() && now.Sub(t.idleSince) >= Rest {
			t.work = 0
		}
		t.idleSince = time.Time{}
		if t.workingSince.IsZero() {
			t.workingSince = now
		}
	} else if t.idleSince.IsZero() {
		t.idleSince = now
		if !t.workingSince.IsZero() {
			t.work += max(time.Duration(0), now.Sub(t.workingSince))
			t.workingSince = time.Time{}
		}
	}
	if !working && now.Sub(t.idleSince) >= Rest {
		t.work = 0
	}
	total := t.work
	if working {
		total += now.Sub(t.workingSince)
	}
	t.duty = total >= Duty
	if t.duty {
		reasons = append(reasons, "GPU duty cycle")
	}
	sample.Reasons = reasons
	return sample
}

func rose(before, current *uint64) bool {
	return before != nil && current != nil && *current > *before
}

func HostMemory() (total, available int64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	var seenAvailable bool
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "kB" {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, err = strconv.ParseInt(f[1], 10, 64)
		case "MemAvailable:":
			seenAvailable = true
			available, err = strconv.ParseInt(f[1], 10, 64)
		}
		if err != nil {
			return 0, 0, err
		}
	}
	if total <= 0 || !seenAvailable || available < 0 {
		return 0, 0, fmt.Errorf("invalid host memory counters")
	}
	return total, available, nil
}
