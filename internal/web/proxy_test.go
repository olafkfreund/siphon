package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

func TestClientIPProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		name    string
		proxies []netip.Prefix
		peer    string
		xff     []string
		want    string
	}{
		{"no trusted proxies", nil, "127.0.0.1:1", []string{"198.51.100.1"}, "127.0.0.1"},
		{"untrusted peer with header", trusted, "192.0.2.9:1", []string{"198.51.100.1"}, "192.0.2.9"},
		{"trusted peer, c", trusted, "127.0.0.1:1", []string{"198.51.100.1"}, "198.51.100.1"},
		{"spoof, c", trusted, "127.0.0.1:1", []string{"203.0.113.66, 198.51.100.1"}, "198.51.100.1"},
		{"chain c, p2", trusted, "127.0.0.1:1", []string{"198.51.100.1, 10.0.0.2"}, "198.51.100.1"},
		{"bad entry gives last trusted hop", trusted, "127.0.0.1:1", []string{"198.51.100.1, junk, 10.0.0.2"}, "10.0.0.2"},
		{"every entry trusted gives leftmost", trusted, "127.0.0.1:1", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.3"},
		{"no header gives peer", trusted, "127.0.0.1:1", nil, "127.0.0.1"},
		{"ipv6 /64", trusted, "127.0.0.1:1", []string{"2001:db8:1:2:aaaa::1"}, "2001:db8:1:2::/64"},
		{"ipv4-mapped peer", trusted, "[::ffff:127.0.0.1]:1", []string{"198.51.100.1"}, "198.51.100.1"},
		{"ipv4-mapped entry", trusted, "127.0.0.1:1", []string{"198.51.100.1, ::ffff:10.0.0.2"}, "198.51.100.1"},
		{"two header lines", trusted, "127.0.0.1:1", []string{"203.0.113.66", "198.51.100.1"}, "198.51.100.1"},
		{"entries with ports", trusted, "127.0.0.1:1", []string{"198.51.100.1:5, 10.0.0.2:5"}, "198.51.100.1"},
		{"unix socket peer", trusted, "@", []string{"198.51.100.1"}, "@"},
	}
	for _, c := range cases {
		s := &server{Options: Options{TrustedProxies: c.proxies}}
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		for _, x := range c.xff {
			r.Header.Add("X-Forwarded-For", x)
		}
		if got := s.clientIP(r); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLimiterBehindProxy(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")} })
	login := func(client string) int {
		return e.do("POST", "/login", url.Values{"token": {"bad"}}, func(r *http.Request) { r.Header.Set("X-Forwarded-For", client) }).Code
	}
	for i := 0; i < failBurst; i++ {
		login("198.51.100.1")
	}
	if c := login("198.51.100.1"); c != 429 {
		t.Fatalf("client A: %d, want 429", c)
	}
	if c := login("198.51.100.2"); c != 401 {
		t.Fatalf("client B: %d, want 401", c)
	}
}
