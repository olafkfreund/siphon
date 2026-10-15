package notify

import (
	"context"
	"errors"
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
// follows no redirects. Close the returned func when done. Use it only for
// rawURL's host: the private allowance applies to whatever host a request names.
func GuardedClient(ctx context.Context, cfg *config.Config, rawURL string) (*http.Client, func(), error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || !(u.Scheme == "https" || u.Scheme == "http" && cfg.ServiceEndpoint(rawURL)) {
		return nil, nil, errors.New("bad url")
	}
	if p := u.Port(); p != "" {
		if _, err := strconv.Atoi(p); err != nil {
			return nil, nil, errors.New("bad url")
		}
	}
	private := cfg.ServiceEndpoint(rawURL)
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := source.ResolveAllowed(rctx, u.Hostname(), private); err != nil { // the dial checks again; this gives the reason up front
		return nil, nil, errors.New("the url isn't reachable from Siphon: public addresses only, unless listed in server.services.private_endpoints")
	}
	c := source.HTTPClient(private, timeout, maxResponse)
	return c, c.CloseIdleConnections, nil
}
