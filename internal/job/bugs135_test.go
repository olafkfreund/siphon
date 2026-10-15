package job

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// #135 A1: an MCP source removed while the job waited fails the job, no panic.
func TestRemovedMCPSourceFailsJob(t *testing.T) {
	p := applyPipeline(t, `
server: { sandbox: none, db: DIR/state.db }
agents:
  fix: { kind: codex, command: /bin/true, prompt: x }
`)
	snap := map[string]config.Agent{"fix": {Kind: "codex", Command: "/bin/true", MCP: []string{"gone"}, Prompt: "x"}}
	state, _, out, _ := p.agentExec(context.Background(), p.Config(), store.QueuedJob{ID: 1}, Payload{Action: config.Action{Agent: "fix"}, Agents: snap}, true)
	if state != "failed" || !strings.Contains(out, "mcp source gone no longer exists") {
		t.Fatalf("%s %q", state, out)
	}
}

func hookPost(t *testing.T, h http.Handler, secret, body, delivery string) int {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/hook/gh", strings.NewReader(body))
	r.Header.Set("X-GitHub-Delivery", delivery)
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// #135 C1: a failing rule's error is logged masked.
func TestRuleErrorLogMasked(t *testing.T) {
	p := applyPipeline(t, `
server: { sandbox: none, db: DIR/state.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github, id: header.X-GitHub-Delivery }
rules:
  - { name: broken, source: gh, when: 'int(event.token) > 0', action: { cmd: [echo, never] } }
`)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	tok := "s3cret" // the webhook secret, a configured secret
	if c := hookPost(t, p.Webhooks()("gh"), "s3cret", `{"token":"`+tok+`"}`, "d1"); c != http.StatusAccepted {
		t.Fatalf("status %d", c)
	}
	if !strings.Contains(buf.String(), "webhook rules") || strings.Contains(buf.String(), tok) {
		t.Fatalf("log: %s", buf.String())
	}
}

// #135 A13: a replayed delivery doesn't overwrite the last event.
func TestReplayKeepsLastEvent(t *testing.T) {
	p := applyPipeline(t, `
server: { sandbox: none, db: DIR/state.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github, id: header.X-GitHub-Delivery }
rules:
  - { name: r, source: gh, when: 'true', on: each, id: 'event.n', action: { cmd: [echo, ok] } }
`)
	h := p.Webhooks()("gh")
	for _, d := range []struct{ id, n string }{{"d1", "1"}, {"d2", "2"}, {"d1", "1"}} {
		hookPost(t, h, "s3cret", `{"n":`+d.n+`}`, d.id)
	}
	got, _ := store.SourceEvent(p.Store.DB, "gh")
	if !strings.Contains(got, `"n":2`) {
		t.Fatalf("last event %s, want n=2", got)
	}
}
