package job

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/cred"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

// codexAuth builds a codex auth.json whose access token expires at exp.
func codexAuth(t *testing.T, refresh string, exp time.Time) []byte {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	b, _ := json.Marshal(map[string]any{"tokens": map[string]string{
		"id_token": "x", "access_token": "h." + payload + ".s", "refresh_token": refresh, "account_id": "a",
	}})
	return b
}

// subPipeline: one codex agent on subscription credential "chatgpt", run by
// the stub script body (it sees $CODEX_HOME, stdin and the usual env).
func subPipeline(t *testing.T, stubBody string) (*Pipeline, cred.Store) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+stubBody), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
sources:
  s: { type: http, url: https://example.com/x }
credentials:
  chatgpt: { provider: codex }
agents:
  fix: { kind: codex, command: ` + stub + `, credential: chatgpt, prompt: "fix {{.event.id}}", approve: false }
rules:
  - { name: start, source: s, when: 'true', id: 'event.id', on: each, cooldown: 1ms, action: { agent: fix } }
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
	t.Cleanup(func() { st.Close() })
	cs := cred.StoreFor(cfg)
	if err := cs.Put("chatgpt", "auth.json", codexAuth(t, "refresh-1", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	return New(cfg, st, time.Now), cs
}

func runAgentJob(t *testing.T, p *Pipeline, id int) (state, output string) {
	t.Helper()
	tx, _ := p.Store.DB.Begin()
	jid, err := store.InsertJob(tx, store.Job{Rule: "start", ActionJSON: fmt.Sprintf(`{"action":{"Agent":"fix"},"env":{"event":{"id":%d}}}`, id)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	state, _, output, _ = p.agentExec(context.Background(), store.QueuedJob{ID: jid}, Payload{Action: config.Action{Agent: "fix"}, Env: map[string]any{"event": map[string]any{"id": id}}})
	return state, output
}

// Plan #7 step 5: a token the CLI refreshes during the run lands in the store.
func TestSubscriptionWritebackUpdatesStore(t *testing.T) {
	newAuth := codexAuth(t, "refresh-2", time.Now().Add(48*time.Hour))
	p, cs := subPipeline(t, "cat >/dev/null; printf '%s' '"+string(newAuth)+"' > \"$CODEX_HOME/auth.json\"; echo done\n")
	if state, out := runAgentJob(t, p, 1); state != "done" {
		t.Fatalf("state %s: %s", state, out)
	}
	files, err := cs.Load("chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files["auth.json"]), "refresh-2") {
		t.Fatalf("store not updated by write-back: %s", files["auth.json"])
	}
	if count(t, p, `SELECT count(*) FROM audit WHERE event='credential_refreshed'`) != 1 {
		t.Fatal("refresh not audited")
	}
}

// Concurrency 1 per login: two runs on the same credential never overlap.
func TestSubscriptionRunsSerialisedPerCredential(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "running")
	p, _ := subPipeline(t, fmt.Sprintf(
		"cat >/dev/null; if ! mkdir %q 2>/dev/null; then echo OVERLAP; exit 3; fi; sleep 0.3; rmdir %q; echo ok\n", lock, lock))
	var wg sync.WaitGroup
	results := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); s, o := runAgentJob(t, p, i+10); results[i] = s + ":" + o }()
	}
	wg.Wait()
	for _, r := range results {
		if !strings.HasPrefix(r, "done") {
			t.Fatalf("runs overlapped or failed: %v", results)
		}
	}
}

// A dead login gives an actionable message and an audit row, not a mystery.
func TestSubscriptionAuthFailureNeedsRelogin(t *testing.T) {
	p, _ := subPipeline(t, "cat >/dev/null; echo 'error: invalid_grant: refresh token expired' >&2; exit 1\n")
	state, out := runAgentJob(t, p, 20)
	if state != "failed" || !strings.Contains(out, "credential chatgpt needs re-login") {
		t.Fatalf("state %s output %q", state, out)
	}
	if count(t, p, `SELECT count(*) FROM audit WHERE event='credential_reauth' AND detail='chatgpt'`) != 1 {
		t.Fatal("credential_reauth not audited")
	}
}

// The agent-result event carries kind and result for every runner kind.
func TestAgentResultShape(t *testing.T) {
	p, _ := subPipeline(t, "cat >/dev/null; echo 'final answer'\n")
	p.Cfg.Rules = append(p.Cfg.Rules, config.Rule{Name: "follow", Source: config.AgentResultSource,
		AllowAgentEvents: true, When: `event.kind == "codex" && event.result == "final answer"`,
		Action: config.Action{Cmd: []string{"echo", "followed"}}})
	if state, out := runAgentJob(t, p, 30); state != "done" {
		t.Fatalf("state %s: %s", state, out)
	}
	if count(t, p, `SELECT count(*) FROM jobs WHERE rule='follow'`) != 1 {
		t.Fatal("agent-result event lacks kind/result")
	}
}
