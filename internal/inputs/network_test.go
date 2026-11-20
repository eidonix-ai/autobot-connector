package inputs

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"autobot-connector/internal/wire"
)

func TestInputRejectsLANResolution(t *testing.T) {
	var resolved bool
	client := client(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		resolved = network == "ip" && host == "static.crates.io"
		return []netip.Addr{netip.AddrFrom4([4]byte{192, 168, 1, 1})}, nil
	})
	defer client.CloseIdleConnections()
	manifest := wire.Manifest{Files: []wire.File{{
		URL: "https://static.crates.io/crates/example/example.crate", Path: "file", SHA256: strings.Repeat("0", 64),
	}}}
	_, err := Build(context.Background(), client, manifest, wire.ScratchCap)
	if !resolved || err == nil || !strings.Contains(err.Error(), "address is not public") {
		t.Fatalf("resolve=%t, error=%v", resolved, err)
	}
	t.Log("allowlisted file host resolving to a LAN address refused before dialing")
}

func TestInputRejectsRedirectOutsideList(t *testing.T) {
	c := PublicClient()
	orig, _ := http.NewRequest("GET", "https://github.com/public/repo", nil)
	redirect, _ := http.NewRequest("GET", "https://example.org/redirect", nil)
	if err := c.CheckRedirect(redirect, []*http.Request{orig}); err == nil {
		t.Fatal("accepted redirect outside allowlist")
	}
	t.Log("redirect outside the fixed input hosts refused")
}
