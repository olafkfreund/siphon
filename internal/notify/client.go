package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/source"
)

// timeout bounds one delivery, and maxResponse how much of the answer is read.
const (
	timeout     = 10 * time.Second
	maxResponse = 4 << 10
)

// GuardedClient is the client a channel's messages go out on: public addresses
// only, unless the url's host:port is in server.services.private_endpoints
// (then private, never link-local). It dials the resolved, checked address and
// follows no redirects. Close the returned func when done.
func GuardedClient(cfg *config.Config, rawURL string) (*http.Client, func(), error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil, nil, errors.New("bad url")
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return nil, nil, errors.New("bad url")
		}
	}
	mode := source.Public
	if cfg.ServiceEndpoint(rawURL) {
		mode = source.PrivateNoLinkLocal
	}
	rctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	addrs, err := source.ResolveAllowedMode(rctx, u.Hostname(), mode)
	if err != nil {
		return nil, nil, errors.New("the url isn't reachable from Siphon: public addresses only, unless listed in server.services.private_endpoints")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var last error
		for _, a := range addrs {
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), strconv.Itoa(port)))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, tr.CloseIdleConnections, nil
}
