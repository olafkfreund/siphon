package job

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
	"github.com/olafkfreund/siphon/internal/web"
)

// Signed webhook → job, through the real web handler and pipeline. Replaying
// the same signed body with a fresh (unsigned) delivery id is rejected.
func TestWebhookEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGW_HOOK", "s3cret")
	t.Setenv("AGW_TOKEN", "tok-0123456789abcdef0123456789abcdef")
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db, token: env:AGW_TOKEN }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github, id: header.X-GitHub-Delivery }
rules:
  - name: pr
    source: gh
    when: 'event.action == "opened"'
    id: 'event.number'
    on: each
    action: { cmd: [echo, "pr {{.event.number}}"] }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := New(cfg, st, time.Now)
	h := web.New(web.Options{Token: "tok", Store: st, Config: p.Config, Decide: p.Decide, Hooks: p.Webhooks(), Now: time.Now})

	body := `{"action":"opened","number":1700000001}`
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	post := func(delivery, sig string) int {
		r := httptest.NewRequest(http.MethodPost, "/hook/gh", strings.NewReader(body))
		r.Header.Set("X-GitHub-Delivery", delivery)
		r.Header.Set("X-Hub-Signature-256", sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if c := post("d1", "sha256=00"); c != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", c)
	}
	if c := post("d1", good); c != http.StatusAccepted {
		t.Fatalf("good webhook: %d", c)
	}
	if c := post("d2-forged", good); c != http.StatusConflict {
		t.Fatalf("replay with new delivery id: %d, want 409", c)
	}
	if _, err := p.RunQueued(t.Context()); err != nil {
		t.Fatal(err)
	}
	var out string
	if err := st.DB.QueryRow(`SELECT output FROM jobs WHERE state='done'`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "pr 1700000001" {
		t.Fatalf("job output %q", out)
	}
}

// Phase 2 review M3: a per-rule evaluation error after commit must not turn an
// accepted webhook into a 500. M4: a rule disabled at runtime never fires.
func TestWebhookRuleErrorAndDisabledRule(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGW_HOOK", "s3cret")
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
rules:
  - { name: ok, source: gh, when: 'true', on: each, id: 'event.n', action: { cmd: [echo, ok] } }
  - { name: broken, source: gh, when: 'int(event.title) > 0', action: { cmd: [echo, never] } }
  - { name: off, source: gh, when: 'true', on: each, id: 'event.n', action: { cmd: [echo, off] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.SetRuleOverride(st.DB, "off", false, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	p := New(cfg, st, time.Now)
	h := p.Webhooks()("gh")

	body := `{"n":1,"title":"not-a-number"}`
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/hook/gh", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 despite the broken rule", w.Code)
	}
	var rules []string
	rows, err := st.DB.Query(`SELECT rule FROM jobs ORDER BY rule`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		rows.Scan(&r)
		rules = append(rules, r)
	}
	if strings.Join(rules, ",") != "ok" {
		t.Fatalf("jobs for rules %v, want only [ok] (broken errors, off is disabled)", rules)
	}
}

// P1: the webhook rate limits belong to the source in the Pipeline, so they
// count across requests (the handler is rebuilt per request), don't leak
// between sources, and an unauthenticated flood is cut off before verification.
func TestWebhookRateLimitPerSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGW_HOOK", "s3cret")
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
sources:
  a: { type: webhook, secret: env:AGW_HOOK, signature: github }
  b: { type: webhook, secret: env:AGW_HOOK, signature: github }
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A frozen clock: the limiter refills 10/s on its clock, so on a busy
	// machine 20 real-time requests could earn back tokens mid-test.
	now := time.Now()
	p := New(cfg, st, func() time.Time { return now })
	post := func(src, body, sig string) int {
		if sig == "" {
			mac := hmac.New(sha256.New, []byte("s3cret"))
			mac.Write([]byte(body))
			sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		r := httptest.NewRequest(http.MethodPost, "/hook/"+src, strings.NewReader(body))
		r.Header.Set("X-Hub-Signature-256", sig)
		w := httptest.NewRecorder()
		p.Webhooks()(src).ServeHTTP(w, r) // a fresh handler each time, as the server does
		return w.Code
	}
	for i := 0; i < 20; i++ {
		if c := post("a", fmt.Sprintf(`{"n":%d}`, i), ""); c != http.StatusAccepted {
			t.Fatalf("request %d: %d", i+1, c)
		}
	}
	if c := post("a", `{"n":21}`, ""); c != http.StatusTooManyRequests {
		t.Fatalf("21st request: %d, want 429", c)
	}
	if c := post("b", `{"n":1}`, ""); c != http.StatusAccepted {
		t.Fatalf("source b shares a's limit: %d", c)
	}
	// An unauthenticated flood on b hits the pre-verification limit.
	got429 := false
	for i := 0; i < 200 && !got429; i++ {
		got429 = post("b", `{}`, "sha256=00") == http.StatusTooManyRequests
	}
	if !got429 {
		t.Fatal("bad-signature flood never limited")
	}
}
