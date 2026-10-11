package action

import (
	"crypto/subtle"
	"errors"
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
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.URL.Path, pr.Out.URL.RawPath = target.Scheme, target.Host, target.Path, target.RawPath
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = target.Host
		},
		FlushInterval: -1,
		Transport:     source.StreamClient(s.AllowPrivate).Transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			msg := string(Mask([]byte(err.Error()), mask))
			if len(msg) > 200 {
				msg = msg[:200]
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
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
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
