package agentloop

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// newClient returns an HTTP client that tunnels every target (http and https)
// with CONNECT through HTTPS_PROXY, the sandbox's only way out. Without
// HTTPS_PROXY it dials directly (sandbox: none).
func newClient() *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second}
	return &http.Client{Transport: &http.Transport{
		Proxy: nil, // never the env proxy: plain-http targets must be tunnelled too
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			p := os.Getenv("HTTPS_PROXY")
			// NO_PROXY lists host:port pairs to reach directly (the MCP bridge forwarders).
			if p == "" || slices.Contains(strings.Split(os.Getenv("NO_PROXY"), ","), addr) {
				return d.DialContext(ctx, network, addr)
			}
			u, err := url.Parse(p)
			if err != nil || u.Host == "" {
				return nil, fmt.Errorf("bad HTTPS_PROXY")
			}
			conn, err := d.DialContext(ctx, "tcp", u.Host)
			if err != nil {
				return nil, err
			}
			auth := ""
			if u.User != nil {
				pw, _ := u.User.Password()
				auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw)) + "\r\n"
			}
			if dl, ok := ctx.Deadline(); ok {
				conn.SetDeadline(dl)
			}
			fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", addr, addr, auth)
			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
			if err != nil || resp.StatusCode != http.StatusOK {
				conn.Close()
				if err == nil {
					err = fmt.Errorf("proxy refused %s: %s", addr, resp.Status)
				}
				return nil, err
			}
			conn.SetDeadline(time.Time{})
			if br.Buffered() > 0 { // a server never speaks first here; keep the bytes anyway
				return &bufConn{Conn: conn, r: br}, nil
			}
			return conn, nil
		},
		ResponseHeaderTimeout: 10 * time.Minute, // local models can be slow
	}}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
