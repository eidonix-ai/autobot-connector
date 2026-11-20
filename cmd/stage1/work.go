package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func keeper() {
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 4, 0, 0, 0, 0, 0) // PR_SET_DUMPABLE
	if errno != 0 {
		fmt.Fprintln(os.Stderr, errno)
		os.Exit(1)
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGUSR1, syscall.SIGCHLD)
	for sig := range signals {
		if sig == syscall.SIGUSR1 {
			syscall.Kill(-1, syscall.SIGSTOP)
			syscall.Kill(-1, syscall.SIGKILL)
		}
		for {
			pid, _ := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			if pid <= 0 {
				break
			}
		}
	}
}

func command(args []string) error {
	if syscall.Getpgrp() != os.Getpid() {
		if err := syscall.Setpgid(0, 0); err != nil {
			return err
		}
	}
	file, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(file, args, os.Environ())
}

func profile() error {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	label, err := os.ReadFile("/proc/self/attr/current")
	if err != nil {
		return err
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	p := liveProfile{string(raw), strings.TrimSpace(string(label)), map[string]string{}, map[string]string{}}
	for line := range strings.SplitSeq(string(status), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok {
			p.Status[key] = strings.Join(strings.Fields(value), " ")
		}
	}
	err = filepath.WalkDir("/dev", func(pth string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeDevice == 0 {
			return nil
		}
		f, err := d.Info()
		if err != nil {
			return err
		}
		dev := uint64(f.Sys().(*syscall.Stat_t).Rdev)
		major := (dev >> 8 & 0xfff) | (dev >> 32 & 0xfffff000)
		minor := (dev & 0xff) | (dev >> 12 & 0xffffff00)
		kind := "b"
		if d.Type()&os.ModeCharDevice != 0 {
			kind = "c"
		}
		p.Devices[pth] = fmt.Sprintf("%s %d:%d", kind, major, minor)
		return nil
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(p)
}

func export(paths []string) error {
	root, err := os.OpenRoot("/work")
	if err != nil {
		return err
	}
	defer root.Close()
	tw := tar.NewWriter(os.Stdout)
	var count, size int64
	for _, name := range paths {
		if !fs.ValidPath(name) || name == "." {
			return fmt.Errorf("invalid salvage path %q", name)
		}
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			info, err := root.Lstat(p)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("invalid salvage parent %q", p)
			}
		}
		err = fs.WalkDir(root.FS(), name, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				return fmt.Errorf("unsupported salvage entry %q", p)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			count++
			size += info.Size()
			if count > 128 || size > salvageCap/2 {
				return errors.New("salvage exceeds cap")
			}
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name, header.Format = p, tar.FormatPAX
			if err = tw.WriteHeader(header); err != nil || info.IsDir() {
				return err
			}
			f, err := root.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func usageError() error { return errors.New("invalid probe arguments") }

func probe(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "memory":
		if len(args) != 2 {
			return usageError()
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return usageError()
		}
		p, err := syscall.Mmap(-1, 0, n<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
		if err != nil {
			return err
		}
		for i := 0; i < len(p); i += os.Getpagesize() {
			p[i] = 1
		}
		fmt.Printf("touched %d MiB\n", n)
		time.Sleep(90 * time.Second)
	case "cpu":
		end := time.Now().Add(3 * time.Second)
		spin := make(chan struct{})
		for range 4 {
			go func() {
				for time.Now().Before(end) {
				}
				spin <- struct{}{}
			}()
		}
		for range 4 {
			<-spin
		}
	case "fork":
		var children []*exec.Cmd
		defer func() {
			for _, c := range children {
				c.Process.Kill()
				c.Wait()
			}
		}()
		for {
			c := exec.Command("/bin/sleep", "20")
			if err := c.Start(); err != nil {
				if !errors.Is(err, syscall.EAGAIN) {
					return err
				}
				fmt.Printf("fork refused with EAGAIN after %d children\n", len(children))
				return nil
			}
			children = append(children, c)
		}
	case "orphan":
		c := exec.Command("/bin/sleep", "60")
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			return err
		}
		fmt.Printf("started detached child %d\n", c.Process.Pid)
		return c.Process.Release()
	case "fill":
		if len(args) != 3 {
			return usageError()
		}
		return fill(args[1], args[2])
	case "connect":
		if len(args) != 3 {
			return usageError()
		}
		return connect(args[1], args[2])
	case "listen":
		if len(args) != 2 {
			return usageError()
		}
		server, err := net.Listen("tcp4", args[1])
		if err != nil {
			return err
		}
		defer server.Close()
		for {
			conn, err := server.Accept()
			if err != nil {
				return err
			}
			conn.Write([]byte("stage1"))
			conn.Close()
		}
	default:
		return usageError()
	}
	return nil
}

func fill(dir, kind string) error {
	var capacity syscall.Statfs_t
	if err := syscall.Statfs(dir, &capacity); err != nil {
		return err
	}
	var written int
	var err error
	switch kind {
	case "bytes":
		var file *os.File
		file, err = os.Create(filepath.Join(dir, "fill"))
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		defer file.Close()
		block := make([]byte, 1<<20)
		for written <= int(capacity.Blocks)*int(capacity.Bsize) && err == nil {
			var n int
			n, err = file.Write(block)
			written += n
		}
	case "inodes":
		for written <= int(capacity.Files) {
			name := filepath.Join(dir, strconv.Itoa(written))
			var file *os.File
			file, err = os.OpenFile(name, os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				break
			}
			file.Close()
			written++
		}
		for i := range written {
			os.Remove(filepath.Join(dir, strconv.Itoa(i)))
		}
	default:
		return usageError()
	}
	if !errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%s %s did not hit ENOSPC: %d, %v", dir, kind, written, err)
	}
	fmt.Printf("%s %s: ENOSPC after %d\n", dir, kind, written)
	return nil
}

func connect(protocol, address string) error {
	network := protocol
	if protocol == "dns" {
		network = "udp4"
	}
	conn, err := net.DialTimeout(network, address, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if protocol == "dns" {
		// One DNS A query for the fixture name.
		query := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 6, 's', 't', 'a', 'g', 'e', '1', 0, 0, 1, 0, 1}
		if _, err := conn.Write(query); err != nil {
			return err
		}
		reply := make([]byte, 512)
		n, err := conn.Read(reply)
		if err != nil {
			return err
		}
		if n < len(query) || reply[0] != 0x12 || reply[1] != 0x34 || reply[2]&0x80 == 0 {
			return errors.New("invalid DNS response")
		}
	} else {
		reply := make([]byte, 6)
		if _, err := io.ReadFull(conn, reply); err != nil {
			return err
		}
		if string(reply) != "stage1" {
			return errors.New("invalid TCP response")
		}
	}
	fmt.Printf("%s control replied\n", protocol)
	return nil
}
