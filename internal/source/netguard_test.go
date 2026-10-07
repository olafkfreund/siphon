package source

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestResolveAllowed(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		host         string
		allowPrivate bool
		wantError    bool
	}{
		{"127.0.0.1", false, true},
		{"127.0.0.1", true, false},
		{"1.1.1.1", false, false},
		{"169.254.169.254", true, true}, // allow_private never reaches metadata
		{"fe80::1", true, true},
	} {
		addrs, err := ResolveAllowed(ctx, tt.host, tt.allowPrivate)
		if (err != nil) != tt.wantError {
			t.Fatalf("ResolveAllowed(%q, %v): %v", tt.host, tt.allowPrivate, err)
		}
		if err == nil && (len(addrs) != 1 || addrs[0].String() != tt.host) {
			t.Fatalf("ResolveAllowed(%q, %v) = %v", tt.host, tt.allowPrivate, addrs)
		}
	}
}

func TestBlockedIP(t *testing.T) {
	for _, tt := range []struct {
		ip      string
		blocked bool
	}{
		{"169.254.169.254", true},
		{"10.1.2.3", true},
		{"127.0.0.1", true},
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"fe80::1", true},
		{"::ffff:169.254.169.254", true},
		{"64:ff9b::101:101", true},
		{"64:ff9b:1::101:101", true},
		{"2002:0101:0101::", true},
		{"::101:101", true},
		{"192.0.0.1", true},
		{"198.18.0.1", true},
		{"168.63.129.16", true},
		{"100.100.100.200", true},
		{"fd00:ec2::254", true},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
	} {
		t.Run(tt.ip, func(t *testing.T) {
			addr := netip.MustParseAddr(tt.ip)
			if got := blockedIP(addr, false); got != tt.blocked {
				t.Fatalf("blockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
			if blockedIP(addr, true) {
				t.Fatalf("blockedIP(%s, allowPrivate=true) = true", tt.ip)
			}
		})
	}
}

func TestGuardedClientDoesNotRedirect(t *testing.T) {
	client := guardedClient(true, time.Second, 100, false)
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("client follows redirects")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	listener.Close()
	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			redirected = true
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer server.Close()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || redirected {
		t.Fatalf("status=%d redirected=%v", resp.StatusCode, redirected)
	}
}

func TestResolveAllowedMode(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		host string
		mode Mode
		ok   bool
	}{
		{"1.1.1.1", Public, true},
		{"10.0.0.5", Public, false},
		{"10.0.0.5", PrivateNoLinkLocal, true},
		{"127.0.0.1", PrivateNoLinkLocal, true},
		{"192.168.1.1", PrivateNoLinkLocal, true},
		{"169.254.169.254", PrivateNoLinkLocal, false},
		{"169.254.0.1", PrivateNoLinkLocal, false},
		{"::ffff:169.254.169.254", PrivateNoLinkLocal, false},
		{"fe80::1", PrivateNoLinkLocal, false},
		{"fd00:ec2::254", PrivateNoLinkLocal, false},
		{"168.63.129.16", PrivateNoLinkLocal, false},
		{"169.254.169.254", Private, true},
		{"64:ff9b::a9fe:a9fe", PrivateNoLinkLocal, false}, // NAT64 of 169.254.169.254
		{"64:ff9b::a00:5", PrivateNoLinkLocal, true},      // NAT64 of 10.0.0.5
		{"64:ff9b:1:a9fe:a9:fe00::", PrivateNoLinkLocal, false},
		{"64:ff9b:1:a00:0:500::", PrivateNoLinkLocal, true},
		{"0.0.0.0", PrivateNoLinkLocal, false},
		{"::", PrivateNoLinkLocal, false},
		{"224.0.0.1", PrivateNoLinkLocal, false},
		{"ff02::1", PrivateNoLinkLocal, false},
	} {
		if _, err := ResolveAllowedMode(ctx, tt.host, tt.mode); (err == nil) != tt.ok {
			t.Errorf("%s mode %d: err %v, want ok=%v", tt.host, tt.mode, err, tt.ok)
		}
	}
}
