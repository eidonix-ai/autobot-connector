package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestPowercapMask(t *testing.T) {
	var base strings.Builder
	for path, rule := range systemMounts {
		fmt.Fprintf(&base, "1 1 0:1 %s %s %s - %s none %s\n", rule.root, path, rule.mode, rule.kind, rule.mode)
	}
	for _, file := range []string{"resolv.conf", "hostname", "hosts"} {
		fmt.Fprintf(&base, "1 1 0:1 /var/lib/docker/containers/test/%s /etc/%s ro - ext4 none ro\n", file, file)
	}
	for path, m := range scratchMounts() {
		flags := "rw,nosuid,nodev"
		if !m.exec {
			flags += ",noexec"
		}
		fmt.Fprintf(&base, "1 1 0:1 / %s %s - tmpfs none rw,size=%dk,nr_inodes=%d,uid=1000,gid=1000,mode=700\n",
			path, flags, m.kib, m.inodes)
	}
	mask := "1 1 0:1 / " + powercapMask + " ro - tmpfs none ro,size=4k,nr_inodes=1\n"
	for _, table := range []string{base.String(), base.String() + mask} {
		if err := checkMounts(table, "test", cdiSpec{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{strings.Replace(mask, " ro -", " rw -", 1), strings.Replace(mask, "size=4k", "size=8k", 1), mask + mask} {
		if err := checkMounts(base.String()+bad, "test", cdiSpec{}); err == nil {
			t.Fatalf("accepted mount %q", bad)
		}
	}
}
