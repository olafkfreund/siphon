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

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
	"github.com/olafkfreund/MCP-AgentGateway/internal/web"
)

// Signed webhook → job, through the real web handler and pipeline. Replaying
// the same signed body with a fresh (unsigned) delivery id is rejected.
func TestWebhookEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGW_HOOK", "s3cret")
	t.Setenv("AGW_TOKEN", "tok")
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
