package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"time"
)

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("::/96"),
}

func guardedClient(allowPrivate bool, timeout time.Duration, maxBody int64, stream bool) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := ResolveAllowed(ctx, host, allowPrivate)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
	}
	if stream {
		return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	return &http.Client{Transport: limitedTransport{base: transport, limit: maxBody}, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Mode is how much of the private range a guarded dial may reach.
type Mode int

const (
	Public             Mode = iota // global unicast only
	Private                        // anything (allow_private)
	PrivateNoLinkLocal             // private, loopback and LAN, but never link-local or metadata
)

// metadataAddrs are cloud metadata endpoints outside the link-local ranges.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("168.63.129.16"),
	netip.MustParseAddr("100.100.100.200"),
}

// ResolveAllowed resolves host and rejects it if any returned address is blocked.
func ResolveAllowed(ctx context.Context, host string, allowPrivate bool) ([]netip.Addr, error) {
	if allowPrivate {
		return ResolveAllowedMode(ctx, host, Private)
	}
	return ResolveAllowedMode(ctx, host, Public)
}

// ResolveAllowedMode is ResolveAllowed with a Mode.
func ResolveAllowedMode(ctx context.Context, host string, mode Mode) ([]netip.Addr, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("no address found")
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip.IP)
		if !ok {
			return nil, errors.New("invalid resolved IP")
		}
		addr = addr.Unmap()
		if blockedMode(addr, mode) {
			return nil, fmt.Errorf("blocked address: %s", addr)
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

func blockedMode(addr netip.Addr, mode Mode) bool {
	if mode == PrivateNoLinkLocal {
		return blockedNoLinkLocal(addr.Unmap(), true)
	}
	return blockedIP(addr, mode == Private)
}

var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local     = netip.MustParsePrefix("64:ff9b:1::/48")
)

// blockedNoLinkLocal refuses link-local, metadata, unspecified and multicast
// addresses, and NAT64 forms that embed one (a NAT64 gateway would reach it).
func blockedNoLinkLocal(addr netip.Addr, nat bool) bool {
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || slices.Contains(metadataAddrs, addr) {
		return true
	}
	if nat && addr.Is6() {
		b := addr.As16()
		switch {
		case nat64WellKnown.Contains(addr): // RFC 6052: v4 in the last 32 bits
			return blockedNoLinkLocal(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), false)
		case nat64Local.Contains(addr): // RFC 6052 /48: v4 in bytes 6,7,9,10
			return blockedNoLinkLocal(netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}), false)
		}
	}
	return false
}

func blockedIP(addr netip.Addr, allowPrivate bool) bool {
	if allowPrivate {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

type limitedTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, t.limit+1), resp.Body}
	return resp, nil
}
