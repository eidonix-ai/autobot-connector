package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	CPUQuota    = 100000
	memoryLimit = 512 << 20
	pidsLimit   = 48
	salvageCap  = 1 << 20
	cdiDevice   = "flare.local/gpu=0"
	cdiPath     = "/profile/cdi.json"
)

var tmpfs = map[string]string{
	"/work":    "rw,exec,nosuid,nodev,size=16m,nr_inodes=128,mode=0700,uid=1000,gid=1000",
	"/tmp":     "rw,noexec,nosuid,nodev,size=8m,nr_inodes=128,mode=0700,uid=1000,gid=1000",
	"/dev/shm": "rw,noexec,nosuid,nodev,size=8m,nr_inodes=128,mode=0700,uid=1000,gid=1000",
}

func (d engine) create(image, lease string) (string, error) {
	var result struct{ ID string }
	err := d.call("POST", "/containers/create", map[string]any{
		"Image": image, "User": "1000:1000", "WorkingDir": "/work",
		"Entrypoint": []string{"/stage1", "keeper"}, "Labels": map[string]string{leaseLabel: lease},
		"HostConfig": map[string]any{
			"NetworkMode": "none", "ReadonlyRootfs": true, "Tmpfs": tmpfs,
			"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"},
			"DeviceRequests": []map[string]any{{"Driver": "cdi", "DeviceIDs": []string{cdiDevice}}},
			"CpuPeriod":      CPUQuota, "CpuQuota": CPUQuota, "Memory": memoryLimit, "MemorySwap": memoryLimit,
			"PidsLimit": pidsLimit, "CgroupnsMode": "private",
			"LogConfig": map[string]any{"Type": "local", "Config": map[string]string{"max-size": "1m", "max-file": "2"}},
		},
	}, &result)
	return result.ID, err
}

type mount struct {
	root, kind, flags, super string
}

func (m mount) check(value string) bool {
	return strings.Contains(","+m.flags+","+m.super+",", ","+value+",")
}

type mountRule struct{ root, kind, mode string }

var systemMounts = map[string]mountRule{
	"/":                   {"/", "overlay", "ro"},
	"/proc":               {"/", "proc", "rw"},
	"/dev":                {"/", "tmpfs", "rw"},
	"/dev/pts":            {"/", "devpts", "rw"},
	"/sys":                {"/", "sysfs", "ro"},
	"/sys/fs/cgroup":      {"/", "cgroup2", "ro"},
	"/dev/mqueue":         {"/", "mqueue", "rw"},
	"/proc/bus":           {"/bus", "proc", "ro"},
	"/proc/fs":            {"/fs", "proc", "ro"},
	"/proc/irq":           {"/irq", "proc", "ro"},
	"/proc/sys":           {"/sys", "proc", "ro"},
	"/proc/sysrq-trigger": {"/sysrq-trigger", "proc", "ro"},
	"/proc/acpi":          {"/", "tmpfs", "ro"},
	"/proc/asound":        {"/", "tmpfs", "ro"},
	"/proc/interrupts":    {"/null", "tmpfs", "rw"},
	"/proc/kcore":         {"/null", "tmpfs", "rw"},
	"/proc/keys":          {"/null", "tmpfs", "rw"},
	"/proc/latency_stats": {"/null", "tmpfs", "rw"},
	"/proc/scsi":          {"/", "tmpfs", "ro"},
	"/proc/timer_list":    {"/null", "tmpfs", "rw"},
	"/sys/firmware":       {"/", "tmpfs", "ro"},
}

type cdiEdits struct {
	Mounts []struct {
		HostPath, ContainerPath string
		Options                 []string
	}
	DeviceNodes []struct {
		Path         string
		Major, Minor int
	}
}

type cdiSpec struct {
	ContainerEdits cdiEdits
	Devices        []struct {
		Name           string
		ContainerEdits cdiEdits
	}
}

func readCDI(pin string) (cdiSpec, error) {
	var spec cdiSpec
	data, err := os.ReadFile(cdiPath)
	if err != nil {
		return spec, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != pin {
		return spec, fmt.Errorf("CDI spec digest changed")
	}
	err = json.Unmarshal(data, &spec)
	return spec, err
}

func checkMounts(raw, id string, spec cdiSpec) error {
	want := make(map[string]mountRule)
	for path, rule := range systemMounts {
		want[path] = rule
	}
	for _, file := range []string{"resolv.conf", "hostname", "hosts"} {
		want["/etc/"+file] = mountRule{"/var/lib/docker/containers/" + id + "/" + file, "ext4", "ro"}
	}
	for _, m := range spec.ContainerEdits.Mounts {
		host := m.HostPath
		if strings.HasPrefix(host, "/lib/") {
			host = "/usr" + host
		}
		want[m.ContainerPath] = mountRule{host, "ext4", "ro"}
	}
	for path := range tmpfs {
		want[path] = mountRule{"/", "tmpfs", "rw"}
	}
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		l, r := strings.Fields(left), strings.Fields(right)
		if !ok || len(l) < 6 || len(r) != 3 {
			return fmt.Errorf("invalid mountinfo entry")
		}
		path := l[4]
		got := mount{l[3], r[0], l[5], r[2]}
		rule, ok := want[path]
		if !ok || rule.root != got.root || rule.kind != got.kind || !strings.HasPrefix(got.flags+",", rule.mode+",") {
			return fmt.Errorf("unexpected mount %s %+v", path, got)
		}
		delete(want, path)
		if _, ok := tmpfs[path]; ok {
			size := "size=8192k"
			if path == "/work" {
				size = "size=16384k"
			}
			for _, value := range []string{size, "nr_inodes=128", "uid=1000", "gid=1000", "mode=700", "nosuid", "nodev"} {
				if !got.check(value) {
					return fmt.Errorf("%s missing mount option %s", path, value)
				}
			}
			if got.check("noexec") != (path != "/work") {
				return fmt.Errorf("%s has incorrect execution mode", path)
			}
		}
	}
	if len(want) != 0 {
		return fmt.Errorf("missing mounts: %v", want)
	}
	return nil
}

type liveProfile struct {
	Mounts, AppArmor string
	Status           map[string]string
	Devices          map[string]string
}

func (d engine) checkProfile(c container, spec cdiSpec) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := d.fixed(ctx, c.ID, "/stage1", "profile")
	if err != nil {
		return err
	}
	var p liveProfile
	if err = json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.AppArmor != "docker-default (enforce)" || c.AppArmorProfile != "docker-default" {
		return fmt.Errorf("AppArmor profile %q / %q", p.AppArmor, c.AppArmorProfile)
	}
	for k, v := range map[string]string{
		"Uid": "1000 1000 1000 1000", "Gid": "1000 1000 1000 1000",
		"CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000",
		"CapBnd": "0000000000000000", "CapAmb": "0000000000000000", "NoNewPrivs": "1", "Seccomp": "2",
	} {
		if p.Status[k] != v {
			return fmt.Errorf("unexpected %s: %q", k, p.Status[k])
		}
	}
	want := map[string]string{
		"/dev/null": "c 1:3", "/dev/zero": "c 1:5", "/dev/full": "c 1:7", "/dev/random": "c 1:8",
		"/dev/urandom": "c 1:9", "/dev/tty": "c 5:0", "/dev/pts/ptmx": "c 5:2",
	}
	edits := []cdiEdits{spec.ContainerEdits}
	for _, device := range spec.Devices {
		if device.Name == "0" {
			edits = append(edits, device.ContainerEdits)
		}
	}
	for _, edit := range edits {
		for _, node := range edit.DeviceNodes {
			want[node.Path] = fmt.Sprintf("c %d:%d", node.Major, node.Minor)
		}
	}
	if len(want) != len(p.Devices) {
		return fmt.Errorf("unexpected device set: %v", p.Devices)
	}
	for path, node := range want {
		if p.Devices[path] != node {
			return fmt.Errorf("unexpected device %s: %q", path, p.Devices[path])
		}
	}
	if err = checkMounts(p.Mounts, c.ID, spec); err != nil {
		return err
	}
	for file, want := range map[string]string{
		"cpu.max": "100000 100000", "memory.max": strconv.Itoa(memoryLimit), "memory.swap.max": "0", "pids.max": strconv.Itoa(pidsLimit),
	} {
		data, err := os.ReadFile(filepath.Join(cgroup(c.ID), file))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) != want {
			return fmt.Errorf("incorrect cgroup %s: %q", file, data)
		}
	}
	return nil
}
