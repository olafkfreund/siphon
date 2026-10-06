package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func guardedClient(allowPrivate bool, timeout time.Duration, maxBody int64) *http.Client {
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
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, errors.New("no address found")
			}
			for _, ip := range ips {
				addr, ok := netip.AddrFromSlice(ip.IP)
				if !ok {
					return nil, errors.New("invalid resolved IP")
				}
				addr = addr.Unmap()
				if blockedIP(addr, allowPrivate) {
					return nil, fmt.Errorf("blocked address: %s", addr)
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
	}
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	return &http.Client{Transport: limitedTransport{base: transport, limit: maxBody}, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func blockedIP(addr netip.Addr, allowPrivate bool) bool {
	return !allowPrivate && (!addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || cgnat.Contains(addr))
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
