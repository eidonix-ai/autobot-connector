package inputs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

var publicHosts = map[string]bool{
	"github.com":              true,
	"codeload.github.com":     true,
	"static.crates.io":        true,
	"files.pythonhosted.org":  true,
	"huggingface.co":          true,
	"cdn-lfs.huggingface.co":  true,
	"cdn-lfs.hf.co":           true,
	"cas-bridge.xethub.hf.co": true,
}

func publicURL(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || !publicHosts[u.Hostname()] ||
		(u.Host != u.Hostname() && u.Host != u.Hostname()+":443") || u.Fragment != "" {
		return errors.New("input URL outside public host list")
	}
	return nil
}

var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}
var globalIPv6 = netip.MustParsePrefix("2000::/3")

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)

func checkedDial(lookup lookupFunc) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		resolved, err := lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		local, err := net.InterfaceAddrs()
		if err != nil {
			return nil, err
		}

		for _, candidate := range resolved {
			ip := candidate.Unmap()
			public := ip.IsGlobalUnicast() && !ip.IsPrivate() && (!ip.Is6() || globalIPv6.Contains(ip))
			for _, p := range special {
				if p.Contains(ip) {
					public = false
				}
			}
			for _, address := range local {
				if network, ok := address.(*net.IPNet); ok && network.Contains(net.IP(ip.AsSlice())) {
					public = false
				}
			}
			if !public {
				return nil, errors.New("resolved address is not public")
			}
		}
		dialer := net.Dialer{Timeout: 5 * time.Second}
		err = errors.New("no usable public address")
		for _, ip := range resolved {
			var conn net.Conn
			conn, err = dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, fmt.Errorf("dial public host: %w", err)
	}
}

func PublicDial(ctx context.Context, network, address string) (net.Conn, error) {
	return checkedDial(net.DefaultResolver.LookupNetIP)(ctx, network, address)
}

func client(lookup lookupFunc) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           checkedDial(lookup),
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			DisableCompression:    true,
			ResponseHeaderTimeout: 20 * time.Second,
			TLSHandshakeTimeout:   8 * time.Second,
			IdleConnTimeout:       10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many input redirects")
			}
			if req.Method != via[0].Method {
				return errors.New("input redirect changed method")
			}
			return publicURL(req.URL)
		},
	}
}

func PublicClient() *http.Client { return client(net.DefaultResolver.LookupNetIP) }
