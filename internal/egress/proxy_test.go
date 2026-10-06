package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/source"
)

func startProxy(t *testing.T, resolve func(context.Context, string, bool) ([]netip.Addr, error)) *Proxy {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	probe.Close()
	p := New("127.0.0.1:0", resolve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for p.Addr() == "" {
		select {
		case err := <-done:
			t.Fatalf("proxy start: %v", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	return p
}

func raw(t *testing.T, p *Proxy, method, target, auth, extra string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if auth != "" {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(auth)) + "\r\n"
	}
	fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\n%s%s\r\n", method, target, target, auth, extra)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestProxy(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	probe.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	host, portText, _ := net.SplitHostPort(u.Host)
	var port int
	fmt.Sscan(portText, &port)
	p := startProxy(t, source.ResolveAllowed)
	proxyURL, blocked, release := p.Register([]Entry{{Host: host, Port: port, AllowPrivate: true}})
	parsed, _ := url.Parse(proxyURL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(parsed), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	name := parsed.User.Username()
	token, _ := parsed.User.Password()
	if code, body := raw(t, p, "CONNECT", "other.example:443", name+":"+token, ""); code != 403 || strings.Contains(body, token) {
		t.Fatalf("disallowed: %d %q", code, body)
	}
	if blocked()["other.example:443"] != 1 {
		t.Fatalf("blocked = %v", blocked())
	}
	for _, auth := range []string{"", name + ":wrong"} {
		if code, body := raw(t, p, "CONNECT", u.Host, auth, ""); code != 407 || strings.Contains(body, token) {
			t.Fatalf("auth: %d %q", code, body)
		}
	}
	if code, body := raw(t, p, "GET", "http://"+u.Host+"/", name+":"+token, ""); code != 405 || strings.Contains(body, token) {
		t.Fatalf("method: %d %q", code, body)
	}
	release()
	if code, body := raw(t, p, "CONNECT", u.Host, name+":"+token, ""); code != 407 || strings.Contains(body, token) {
		t.Fatalf("released: %d %q", code, body)
	}
}

func TestRules(t *testing.T) {
	resolver := func(_ context.Context, _ string, _ bool) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}
	p := startProxy(t, resolver)
	proxyURL, blocked, _ := p.Register([]Entry{{Host: "*.example.com"}})
	u, _ := url.Parse(proxyURL)
	token, _ := u.User.Password()
	auth := u.User.Username() + ":" + token
	for _, tt := range []struct {
		target string
		want   int
	}{
		{"example.com:443", 403},
		{"a.example.com:444", 403},
		{"a.example.com:443", 403}, // Matches the pattern, then rejects the private IP.
	} {
		code, body := raw(t, p, "CONNECT", tt.target, auth, "")
		if code != tt.want || strings.Contains(body, token) || blocked()[tt.target] != 1 {
			t.Fatalf("%s: status=%d blocked=%v body=%q", tt.target, code, blocked(), body)
		}
	}
	// Only the wildcard subdomain on the default port reaches resolution.
	var resolves atomic.Int32
	p2 := startProxy(t, func(_ context.Context, _ string, _ bool) ([]netip.Addr, error) {
		resolves.Add(1)
		return nil, errors.New("test resolver")
	})
	u2, _, _ := p2.Register([]Entry{{Host: "*.example.com"}})
	parsed, _ := url.Parse(u2)
	pass, _ := parsed.User.Password()
	if code, _ := raw(t, p2, "CONNECT", "a.example.com:443", parsed.User.Username()+":"+pass, ""); code != 403 || resolves.Load() != 1 {
		t.Fatalf("wildcard match status = %d", code)
	}
	if code, _ := raw(t, p2, "CONNECT", "example.com:443", parsed.User.Username()+":"+pass, ""); code != 403 {
		t.Fatalf("wildcard base status = %d", code)
	}
	if code, _ := raw(t, p2, "CONNECT", "a.example.com:444", parsed.User.Username()+":"+pass, ""); code != 403 || resolves.Load() != 1 {
		t.Fatalf("default port status = %d", code)
	}
}

func TestHeadLimit(t *testing.T) {
	p := startProxy(t, source.ResolveAllowed)
	code, _ := raw(t, p, "CONNECT", "example.com:443", "", "X-Large: "+strings.Repeat("x", 8192)+"\r\n")
	if code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized head status = %d", code)
	}
}

// A name whose first address is unreachable (e.g. IPv6 listed first for an
// IPv4-only server) still tunnels via the next checked address.
func TestDialFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	_, portText, _ := net.SplitHostPort(u.Host)
	var port int
	fmt.Sscan(portText, &port)
	p := startProxy(t, func(context.Context, string, bool) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1")}, nil
	})
	proxyURL, _, release := p.Register([]Entry{{Host: "dual.test", Port: port, AllowPrivate: true}})
	defer release()
	parsed, _ := url.Parse(proxyURL)
	token, _ := parsed.User.Password()
	if code, _ := raw(t, p, "CONNECT", "dual.test:"+portText, parsed.User.Username()+":"+token, ""); code != 200 {
		t.Fatalf("CONNECT = %d, want 200 via the second address", code)
	}
}

func TestBlockedCap(t *testing.T) {
	p := New("127.0.0.1:0", nil)
	r := &run{blocked: map[string]int{}}
	for i := 0; i < 200; i++ {
		p.block(r, fmt.Sprintf("h%d.example:443", i))
	}
	p.block(r, "bad\x00\nhost:443")
	if len(r.blocked) != maxBlocked+1 || r.blocked["other"] != 200-maxBlocked+1 {
		t.Fatalf("%d hosts, other=%d", len(r.blocked), r.blocked["other"])
	}
}

func TestServeUnix(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	host, portText, _ := net.SplitHostPort(u.Host)
	var port int
	fmt.Sscan(portText, &port)
	p := New("127.0.0.1:0", source.ResolveAllowed)
	sock := t.TempDir() + "/egress.sock"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.ServeUnix(ctx, sock) }()
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Skipf("unix socket unavailable: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o660 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	proxyURL, _, _ := p.Register([]Entry{{Host: host, Port: port, AllowPrivate: true}})
	pu, _ := url.Parse(proxyURL)
	token, _ := pu.User.Password()
	auth := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + token))
	for target, want := range map[string]int{u.Host: 200, "other.example:443": 403} {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s: %v %v", target, resp, err)
		}
	}
	cancel()
	<-done
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
}
