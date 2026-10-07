package job

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

func applyPipeline(t *testing.T, y string) *Pipeline {
	t.Helper()
	t.Setenv("AGW_HOOK", "s3cret")
	cfg, err := config.Parse([]byte(strings.ReplaceAll(y, "DIR", t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(cfg, st, time.Now)
}

func parseCfg(t *testing.T, p *Pipeline, y string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(strings.ReplaceAll(y, "DIR", p.Config().Server.DB[:len(p.Config().Server.DB)-len("/state.db")])))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func jobCount(p *Pipeline) (n int) {
	p.Store.DB.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n)
	return
}

const applyBase = `
server: { sandbox: none, db: DIR/state.db }
sources:
  s: { type: webhook, secret: env:AGW_HOOK, signature: github }
`

func TestApplyRuleFiresOnNextEvent(t *testing.T) {
	p := applyPipeline(t, applyBase)
	ev := rule.Event{Source: "s", Data: map[string]any{"n": 1}}
	if _, ids, err := p.HandleEvent(context.Background(), ev, false); err != nil || len(ids) != 0 {
		t.Fatalf("%v %v", ids, err)
	}
	next := parseCfg(t, p, applyBase+"rules:\n  - { name: r, source: s, when: 'true', action: { cmd: [echo, hi] } }\n")
	if err := p.Apply(next); err != nil {
		t.Fatal(err)
	}
	if _, ids, err := p.HandleEvent(context.Background(), ev, false); err != nil || len(ids) != 1 {
		t.Fatalf("after Apply: %v %v", ids, err)
	}
}

func TestApplyQueuedJobKeepsSnapshot(t *testing.T) {
	const y = applyBase + `
agents:
  fix: { kind: claude, prompt: "ORIGINAL" }
rules:
  - { name: r, source: s, when: 'true', action: { agent: fix } }
`
	p := applyPipeline(t, y)
	if _, ids, err := p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{}}, false); err != nil || len(ids) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
	if err := p.Apply(parseCfg(t, p, strings.Replace(y, "ORIGINAL", "EDITED", 1))); err != nil {
		t.Fatal(err)
	}
	var payload string
	p.Store.DB.QueryRow(`SELECT action_json FROM jobs`).Scan(&payload)
	pl, err := decodePayload(payload)
	if err != nil || pl.Agents["fix"].Prompt != "ORIGINAL" || p.Config().Agents["fix"].Prompt != "EDITED" {
		t.Fatalf("snapshot %+v %v", pl.Agents, err)
	}
}

func TestApplyWebhookSourceAdded(t *testing.T) {
	p := applyPipeline(t, "server: { sandbox: none, db: DIR/state.db }\n")
	hooks := p.Webhooks()
	if hooks("gh") != nil {
		t.Fatal("unknown source must have no handler")
	}
	next := parseCfg(t, p, `
server: { sandbox: none, db: DIR/state.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github, id: header.X-GitHub-Delivery }
rules:
  - { name: r, source: gh, when: 'true', action: { cmd: [echo, hi] } }
`)
	if err := p.Apply(next); err != nil {
		t.Fatal(err)
	}
	h := hooks("gh") // same lookup func as before the Apply
	if h == nil {
		t.Fatal("no handler after Apply")
	}
	body := `{"n":1}`
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/hook/gh", strings.NewReader(body))
	r.Header.Set("X-GitHub-Delivery", "d1")
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || jobCount(p) != 1 {
		t.Fatalf("code %d jobs %d", w.Code, jobCount(p))
	}
}

func TestApplyRemovedSourceStopsPolling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var reads atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	server.AddResource(&mcp.Resource{Name: "v", URI: "test://v"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			reads.Add(1)
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "test://v", MIMEType: "application/json", Text: `{"value":1}`}}}, nil
		})
	const y = `
server: { sandbox: none, db: DIR/state.db, workers: 1 }
sources:
  m: { type: mcp, command: [unused], read: { resource: "test://v" }, poll: 20ms }
`
	p := applyPipeline(t, y)
	p.MCPTransport = func(string) mcp.Transport {
		s, c := mcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, s, nil); err != nil {
			t.Error(err)
		}
		return c
	}
	served := make(chan error, 1)
	go func() { served <- p.Serve(ctx) }()
	for reads.Load() < 3 {
		select {
		case <-ctx.Done():
			t.Fatal("source never polled")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := p.Apply(parseCfg(t, p, "server: { sandbox: none, db: DIR/state.db, workers: 1 }\n")); err != nil {
		t.Fatal(err)
	}
	n := reads.Load()
	time.Sleep(200 * time.Millisecond) // ten poll intervals
	if got := reads.Load(); got != n {
		t.Fatalf("still polling after removal: %d -> %d", n, got)
	}
	cancel()
	<-served
}

// Until the MCP bridge (plan step 4), an agent never gets a stdio source's env.
func TestAgentRefusesStdioEnv(t *testing.T) {
	t.Setenv("AGW_E", "secret-value")
	p := applyPipeline(t, `
server: { sandbox: none, db: DIR/state.db, mcp_packages: {gh: {command: [srv], env: [GITHUB_TOKEN]}} }
sources:
  m: { type: mcp, package: gh, read: {tool: t}, env: {GITHUB_TOKEN: 'env:AGW_E'} }
agents:
  fix: { kind: codex, command: /bin/true, mcp: [m], prompt: x }
`)
	state, _, out, _ := p.agentExec(context.Background(), p.Config(), store.QueuedJob{ID: 1}, Payload{Action: config.Action{Agent: "fix"}}, true)
	if state != "failed" || !strings.Contains(out, "needs the MCP bridge") || strings.Contains(out, "secret-value") {
		t.Fatalf("%s %q", state, out)
	}
}
