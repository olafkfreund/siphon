package job

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	p := &Pipeline{Cfg: cfg, Store: st, Now: time.Now}
	h := web.New(web.Options{Token: "tok", Store: st, Cfg: cfg, Decide: p.Decide, Hooks: p.Webhooks(), Now: time.Now})

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
	h := p.Webhooks()["gh"]

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
