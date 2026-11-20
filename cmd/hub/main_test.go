package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestClearStaleAdminSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sock")
	if err := clearStaleSocket(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := clearStaleSocket(path); err == nil {
		t.Fatal("regular file was treated as a socket")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := clearStaleSocket(path); err == nil {
		t.Fatal("live listener was treated as stale")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clearStaleSocket(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket still exists: %v", err)
	}
}
