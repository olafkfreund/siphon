package job

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func tickEnv(t *testing.T, h http.HandlerFunc) (*Pipeline, *config.Config) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := &config.Config{Sources: map[string]*config.Source{
		"s": {Type: "http", URL: srv.URL, AllowPrivate: true},
	}}
	cfg.Limits.HTTPTimeout = config.Duration(5 * time.Second)
	cfg.Limits.HTTPMaxBody = 1 << 20
	return newPipeline(t, 1), cfg
}

func sourceErr(t *testing.T, p *Pipeline) (string, bool) {
	st, err := store.SourceStates(p.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := st["s"]
	return s.LastError, ok
}

func TestTickCancelledPollKeepsStatus(t *testing.T) {
	started := make(chan struct{})
	p, cfg := tickEnv(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	if err := store.PutSourceState(p.Store.DB, "s", p.Now(), ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.tick(ctx, cfg, "s"); done <- err }()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("want the poll error")
	}
	if e, ok := sourceErr(t, p); !ok || e != "" {
		t.Fatalf("state changed: %q ok=%v", e, ok)
	}
}

func TestTickOwnFailureStored(t *testing.T) {
	p, cfg := tickEnv(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	if _, err := p.tick(context.Background(), cfg, "s"); err == nil {
		t.Fatal("want an error")
	}
	if e, _ := sourceErr(t, p); e == "" {
		t.Fatal("error not stored")
	}
}
