package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/store"
)

func TestNotifyAPI(t *testing.T) {
	var hits atomic.Int32
	var gotAuth atomic.Value
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		io.Copy(io.Discard, r.Body)
	}))
	defer hook.Close()
	host := strings.TrimPrefix(hook.URL, "http://")
	ce := newCfgEnvFile(t, "server: { sandbox: none, db: DIR/s.db, services: {private_endpoints: ['"+host+"']} }\n")

	// Apply a channel with pasted secrets; they are stored as files, never echoed.
	apply := func(dry bool, secrets string) *httptest.ResponseRecorder {
		p := "/api/config/apply"
		if dry {
			p += "?dry_run=1"
		}
		return ce.api("POST", p, `{"items":[{"kind":"notify","name":"hook","yaml":"type: webhook\nevents: [failed]\n"}],"secrets":`+secrets+`}`)
	}
	sec := `{"notify/hook.url":"` + hook.URL + `/n","notify/hook.token":"tok-123"}`
	if w := apply(true, sec); w.Code != 200 || strings.Contains(w.Body.String(), "tok-123") || strings.Contains(w.Body.String(), hook.URL+"/n") {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if items, _ := store.ConfigItems(ce.st.DB); len(items) != 0 {
		t.Fatal("dry run stored an item")
	}
	if w := apply(false, `{"notify/hook.api_key":"x"}`); w.Code != 422 {
		t.Fatalf("a non-notify secret field: %d %s", w.Code, w.Body.String())
	}
	if w := apply(false, sec); w.Code != 200 {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	b, err := os.ReadFile(filepath.Join(ce.dir, "secrets", "notify--hook+token"))
	if err != nil || string(b) != "tok-123" {
		t.Fatalf("token file: %q %v", b, err)
	}
	if w := ce.api("GET", "/api/config/notify/hook", ""); w.Code != 200 || strings.Contains(w.Body.String(), "tok-123") || strings.Contains(w.Body.String(), hook.URL+"/n") {
		t.Fatalf("get leaks: %d %s", w.Code, w.Body.String())
	}
	// A secret field of another kind may not take `url`.
	if w := ce.api("POST", "/api/config/apply", `{"items":[{"kind":"sources","name":"s","yaml":"type: webhook\n"}],"secrets":{"sources/s.url":"https://x"}}`); w.Code != 422 {
		t.Fatalf("url secret on a source: %d", w.Code)
	}

	// Test sends one message and audits it; the token went as a bearer.
	w := ce.api("POST", "/api/notify/hook/test", "")
	var res struct {
		OK     bool
		Status int
		Error  string
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || !res.OK || res.Status != 200 || hits.Load() != 1 || gotAuth.Load() != "Bearer tok-123" {
		t.Fatalf("test: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
	}
	if w := ce.api("POST", "/api/notify/nope/test", ""); w.Code != 404 {
		t.Fatalf("unknown channel: %d", w.Code)
	}
	// The outbox listing, filtered.
	store.EnqueueNotification(ce.st.DB, store.Notification{Channel: "hook", Event: "failed", Key: "job:1", Title: "t", Body: "b"}, time.Now())
	w = ce.api("GET", "/api/notifications?channel=hook&limit=5", "")
	var rows []map[string]any
	json.Unmarshal(w.Body.Bytes(), &rows)
	if w.Code != 200 || len(rows) != 1 || rows[0]["state"] != "pending" || strings.Contains(w.Body.String(), "tok-123") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("GET", "/api/notifications?channel=other", ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty list: %s", w.Body.String())
	}
	if w := ce.api("GET", "/api/notifications?job=x", ""); w.Code != 400 {
		t.Fatalf("bad job: %d", w.Code)
	}
}
