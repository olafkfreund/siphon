package action

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/olafkfreund/siphon/internal/source"
)

// oauthProxy forwards an agent's MCP requests to an OAuth server, adding a
// fresh access token each time. The agent only holds key.
func oauthProxy(s MCPServer, key string, mask []string) http.Handler {
	target, perr := url.Parse(s.URL)
	tr := source.StreamClient(s.AllowPrivate).Transport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = time.Minute // a stream's headers come first; only a hung server waits this long
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.URL.Path, pr.Out.URL.RawPath = target.Scheme, target.Host, target.Path, target.RawPath
			q := pr.In.URL.Query() // the source url's own query wins
			for k, v := range target.Query() {
				q[k] = v
			}
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Host = target.Host
		},
		FlushInterval: -1,
		Transport:     tr,
		// Not followed, and not handed to the agent, which would then go around the proxy.
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
				return fmt.Errorf("the MCP server redirected (%d) to %q: set the source url to that address", resp.StatusCode, resp.Header.Get("Location"))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			msg := string(Mask([]byte(err.Error()), mask))
			if len(msg) > 300 {
				msg = msg[:300]
			}
			http.Error(w, "upstream: "+msg, http.StatusBadGateway)
		},
	}
	want := []byte("Bearer " + key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/mcp" || perr != nil {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Upgrade") != "" { // a hijacked tunnel would outlive stop
			http.Error(w, "upgrade not supported", http.StatusBadRequest)
			return
		}
		tok, err := s.Token(r.Context())
		if err != nil {
			http.Error(w, string(Mask([]byte(err.Error()), mask)), http.StatusUnauthorized)
			return
		}
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "proxy-") {
				r.Header.Del(k)
			}
		}
		r.Header.Del("Cookie")
		for k, v := range s.Headers {
			r.Header.Set(k, v)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		rp.ServeHTTP(w, r)
	})
}

// startOAuthProxy serves oauthProxy for one run: on a unix socket in a fresh
// run dir (systemd mode, port is 0) or on loopback (sandbox none, sock is "").
func startOAuthProxy(o *AgentOptions, s MCPServer, key string) (sock string, port int, stop func(), err error) {
	h := oauthProxy(s, key, append([]string{key}, o.Secrets...))
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	if o.Sandbox.Mode == "none" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", 0, nil, err
		}
		go srv.Serve(ln)
		return "", ln.Addr().(*net.TCPAddr).Port, func() { srv.Close() }, nil
	}
	_, runDir, gid, err := newRunDir(o.Sandbox.Dir)
	if err != nil {
		return "", 0, nil, err
	}
	sock = filepath.Join(runDir, "mcp.sock")
	tmp := sock + ".tmp"
	ln, err := net.Listen("unix", tmp)
	if err == nil {
		err = os.Chmod(tmp, 0o660)
	}
	if err == nil {
		if cerr := os.Chown(tmp, -1, gid); cerr != nil && !errors.Is(cerr, syscall.EPERM) {
			err = cerr
		}
	}
	if err == nil {
		err = os.Rename(tmp, sock)
	}
	if err != nil {
		if ln != nil {
			ln.Close()
		}
		os.RemoveAll(runDir)
		return "", 0, nil, err
	}
	go srv.Serve(ln)
	return sock, 0, func() { srv.Close(); os.RemoveAll(runDir) }, nil
}
