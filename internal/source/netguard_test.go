package source

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func TestGuardedDial(t *testing.T) {
	for _, host := range []string{"169.254.169.254", "10.1.2.3", "127.0.0.1", "100.100.100.200", "0.0.0.0"} {
		client := guardedClient(false, time.Second, 100)
		_, err := client.Transport.(limitedTransport).base.(*http.Transport).DialContext(context.Background(), "tcp", net.JoinHostPort(host, "80"))
		if err == nil {
			t.Fatalf("accepted %s", host)
		}
	}
	for _, host := range []string{"169.254.169.254", "10.1.2.3"} {
		if blockedIP(netip.MustParseAddr(host), true) {
			t.Fatalf("blocked %s with allowPrivate", host)
		}
	}
}
