package action

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type seen struct {
	mu   sync.Mutex
	n    int
	h    http.Header
	path string
	q    string
}

func (s *seen) handler(extra func(http.ResponseWriter)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.n++
		s.h, s.path, s.q = r.Header.Clone(), r.URL.Path, r.URL.RawQuery
		s.mu.Unlock()
		if extra != nil {
			extra(w)
		}
	})
}

func (s *seen) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }

func proxyGet(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOAuthProxyHandler(t *testing.T) {
	up := &seen{}
	srv := httptest.NewServer(up.handler(nil))
	defer srv.Close()
	tok := "t1"
	var terr error
	h := oauthProxy(MCPServer{URL: srv.URL + "/mcp", AllowPrivate: true, Headers: map[string]string{"X-Static": "s"},
		Token: func(context.Context) (string, error) { return tok, terr }}, "KEY", []string{"KEY", "t2"})
	good := map[string]string{"Authorization": "Bearer KEY"}

	if c := proxyGet(t, h, "/mcp", nil).Code; c != 401 {
		t.Fatalf("no key: %d", c)
	}
	if c := proxyGet(t, h, "/mcp", map[string]string{"Authorization": "Bearer nope"}).Code; c != 401 {
		t.Fatalf("wrong key: %d", c)
	}
	if up.count() != 0 {
		t.Fatal("upstream called without key")
	}
	if c := proxyGet(t, h, "/other", good).Code; c != 404 {
		t.Fatalf("other: %d", c)
	}

	hdr := map[string]string{"Authorization": "Bearer KEY", "Cookie": "a=b", "Proxy-Authorization": "x", "X-Keep": "k"}
	if c := proxyGet(t, h, "/mcp?a=1", hdr).Code; c != 200 {
		t.Fatalf("good: %d", c)
	}
	if up.h.Get("Authorization") != "Bearer t1" || up.h.Get("X-Static") != "s" || up.h.Get("X-Keep") != "k" ||
		up.h.Get("Cookie") != "" || up.h.Get("Proxy-Authorization") != "" || up.q != "a=1" || up.path != "/mcp" {
		t.Fatalf("upstream saw %v %s?%s", up.h, up.path, up.q)
	}

	tok = "t2"
	proxyGet(t, h, "/mcp", good)
	if up.h.Get("Authorization") != "Bearer t2" {
		t.Fatalf("token not refreshed: %v", up.h)
	}

	before := up.count()
	terr = errors.New("login expired t2")
	w := proxyGet(t, h, "/mcp", good)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "login expired") || strings.Contains(w.Body.String(), "t2") || up.count() != before {
		t.Fatalf("token error: %d %q upstream %d->%d", w.Code, w.Body, before, up.count())
	}
}

func TestOAuthProxySSEFlushes(t *testing.T) {
	release := make(chan struct{})
	up := &seen{}
	srv := httptest.NewServer(up.handler(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/octet-stream") // event-stream is flushed by ReverseProxy anyway
		w.Write([]byte("data: 1\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	front := httptest.NewServer(oauthProxy(MCPServer{URL: srv.URL + "/mcp", AllowPrivate: true,
		Token: func(context.Context) (string, error) { return "t", nil }}, "KEY", nil))
	defer front.Close()
	req, _ := http.NewRequest("GET", front.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer KEY")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(resp.Body).ReadString('\n'); line <- l }()
	select {
	case l := <-line:
		if l != "data: 1\n" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not flushed before upstream finished")
	}
}

func TestStartOAuthProxyNone(t *testing.T) {
	up := &seen{}
	srv := httptest.NewServer(up.handler(nil))
	defer srv.Close()
	o := &AgentOptions{Sandbox: SandboxOptions{Mode: "none"}}
	sock, port, stop, err := startOAuthProxy(o, MCPServer{URL: srv.URL + "/mcp", AllowPrivate: true,
		Token: func(context.Context) (string, error) { return "t", nil }}, "KEY")
	if err != nil || sock != "" || port == 0 {
		t.Fatalf("%q %d %v", sock, port, err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	req, _ := http.NewRequest("POST", "http://"+addr+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer KEY")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
	stop()
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("port still open after stop")
	}
}

func TestStartOAuthProxyUnix(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "oap")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	up := &seen{}
	srv := httptest.NewServer(up.handler(nil))
	defer srv.Close()
	o := &AgentOptions{Sandbox: SandboxOptions{Mode: "systemd", Dir: dir}}
	sock, _, stop, err := startOAuthProxy(o, MCPServer{URL: srv.URL + "/mcp", AllowPrivate: true,
		Token: func(context.Context) (string, error) { return "t", nil }}, "KEY")
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest("POST", "http://x/mcp", nil)
	req.Header.Set("Authorization", "Bearer KEY")
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
	stop()
	if _, err := os.Stat(sock); err == nil {
		t.Fatal("socket left after stop")
	}
}

func TestStartBridgesOAuthProxy(t *testing.T) {
	o := &AgentOptions{Sandbox: SandboxOptions{Mode: "none"}, MCP: map[string]MCPServer{"m1": {
		URL: "https://mcp.example/mcp", Token: func(context.Context) (string, error) { return "t", nil }}}}
	forwards, direct, stop, err := startBridges(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	m := o.MCP["m1"]
	if forwards != nil || len(direct) != 1 || m.Token != nil || !strings.HasPrefix(m.URL, "http://127.0.0.1:") || strings.Contains(m.URL, "example") {
		t.Fatalf("%v %v %+v", forwards, direct, m)
	}
	key := strings.TrimPrefix(m.Headers["Authorization"], "Bearer ")
	if key == "" || len(o.Secrets) != 1 || o.Secrets[0] != key {
		t.Fatalf("key %q secrets %v", key, o.Secrets)
	}
}
