package source

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// #135 C4: polls must not leave one idle connection each.
func TestPollsDoNotLeakConnections(t *testing.T) {
	var open atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	srv.Start()
	defer srv.Close()
	s := HTTP{Options: HTTPOptions{Name: "x", URL: srv.URL, AllowPrivate: true}}
	for i := 0; i < 50; i++ {
		if _, err := s.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100 && open.Load() > 1; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if n := open.Load(); n > 1 {
		t.Fatalf("%d connections still open after 50 polls", n)
	}
}
