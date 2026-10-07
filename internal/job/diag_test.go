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
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

func TestRuleErrorRecordedAndCleared(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGW_HOOK", "s3cret")
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
  other: { type: webhook, secret: env:AGW_HOOK, signature: github }
rules:
  - { name: broken, source: gh, when: 'int(event.title) > 0', action: { cmd: [echo, never] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := New(cfg, st, time.Now)
	post := func(src, body, sig string) int {
		mac := hmac.New(sha256.New, []byte("s3cret"))
		mac.Write([]byte(body))
		if sig == "" {
			sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		r := httptest.NewRequest(http.MethodPost, "/hook/"+src, strings.NewReader(body))
		r.Header.Set("X-Hub-Signature-256", sig)
		w := httptest.NewRecorder()
		p.Webhooks()(src).ServeHTTP(w, r)
		return w.Code
	}
	if c := post("gh", `{"title":"nope"}`, ""); c != 202 {
		t.Fatalf("%d", c)
	}
	msg, at, err := store.RuleError(st.DB, "broken")
	if err != nil || msg == "" || at.IsZero() {
		t.Fatalf("not recorded: %q %v", msg, err)
	}
	// An event from another source does not evaluate the rule, so does not clear it.
	if c := post("other", `{"title":"3"}`, ""); c != 202 {
		t.Fatalf("%d", c)
	}
	if m, _, _ := store.RuleError(st.DB, "broken"); m == "" {
		t.Fatal("cleared by an unrelated source")
	}
	if c := post("gh", `{"title":"5"}`, ""); c != 202 {
		t.Fatalf("%d", c)
	}
	if m, _, _ := store.RuleError(st.DB, "broken"); m != "" {
		t.Fatalf("not cleared: %q", m)
	}

	// A refused delivery is recorded with a status and reason, nothing from the request.
	if c := post("gh", `{"secret-looking":"SENSITIVE-BODY"}`, "sha256=00"); c != 401 {
		t.Fatalf("%d", c)
	}
	d, err := store.SourceDiagnostics(st.DB, "gh")
	if err != nil || d.RejectAt == nil || !strings.Contains(d.Reject, "401") {
		t.Fatalf("%+v %v", d, err)
	}
	var all string
	st.DB.QueryRow(`SELECT json || last_reject FROM source_state WHERE source='gh'`).Scan(&all)
	for _, bad := range []string{"SENSITIVE-BODY", "sha256=00", "s3cret"} {
		if strings.Contains(d.Reject, bad) {
			t.Fatalf("reject reason holds %q", bad)
		}
	}
}

// A secret quoted in an evaluation error is masked before it is stored.
func TestRuleErrorMasked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGW_HOOK", "s3cret-hook-value")
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
rules:
  - { name: leaky, source: gh, when: 'int("s3cret-hook-value") > 0', action: { cmd: [echo] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := New(cfg, st, time.Now)
	if _, _, _, _, err := p.handleEvent(t.Context(), cfg, rule.Event{Source: "gh", Data: map[string]any{}}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	msg, _, _ := store.RuleError(st.DB, "leaky")
	if msg == "" || strings.Contains(msg, "s3cret-hook-value") {
		t.Fatalf("stored %q", msg)
	}
}
