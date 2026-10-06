package job

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/rule"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

// guardPipeline builds a pipeline whose agent runner is a stub printing {"ok":true}.
func guardPipeline(t *testing.T, extra string) *Pipeline {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "runner.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '{\"ok\":true}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
limits: { agent_runs_per_day: 2 }
sources:
  s: { type: http, url: https://example.com/x }
agents:
  fix: { runner: [` + stub + `], prompt: "fix {{.event.id}}", approve: false }
rules:
  - name: start
    source: s
    when: 'true'
    id: 'event.id'
    on: each
    cooldown: 1s
    action: { agent: fix }
  - name: ignores-agents
    source: agent-result
    when: 'true'
    action: { cmd: [echo, ignored] }
  - name: follows-agents
    source: agent-result
    allow_agent_events: true
    when: 'event.ok'
    action: { cmd: [echo, "followed"] }
` + extra))
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
	now := time.Unix(1_700_000_000, 0)
	return &Pipeline{Cfg: cfg, Store: st, Now: func() time.Time { now = now.Add(time.Minute); return now }}
}

func count(t *testing.T, p *Pipeline, q string) int {
	t.Helper()
	var n int
	if err := p.Store.DB.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Plan step 11: agent result re-enters as agent-result at depth+1 and only
// opted-in rules see it.
func TestAgentResultLoopGuard(t *testing.T) {
	p := guardPipeline(t, "")
	ctx := context.Background()
	if _, _, err := p.HandleEvent(ctx, rule.Event{Source: "s", Data: map[string]any{"id": 1}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RunQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, p, `SELECT count(*) FROM jobs WHERE state='done'`); n != 2 {
		t.Fatalf("done jobs = %d, want 2 (agent + follows-agents)", n)
	}
	if n := count(t, p, `SELECT count(*) FROM jobs WHERE rule='follows-agents' AND depth=1 AND output LIKE 'followed%'`); n != 1 {
		t.Fatalf("follows-agents depth-1 job missing")
	}
	if n := count(t, p, `SELECT count(*) FROM jobs WHERE rule='ignores-agents'`); n != 0 {
		t.Fatalf("rule without allow_agent_events saw an agent result")
	}
}

func TestDepthCap(t *testing.T) {
	p := guardPipeline(t, "")
	ev := rule.Event{Source: config.AgentResultSource, Data: map[string]any{"ok": true}, Depth: config.MaxDepth + 1}
	_, ids, err := p.HandleEvent(context.Background(), ev, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 || count(t, p, `SELECT count(*) FROM jobs`) != 0 {
		t.Fatalf("depth %d enqueued jobs %v", ev.Depth, ids)
	}
	if count(t, p, `SELECT count(*) FROM audit WHERE event='skip_depth'`) != 1 {
		t.Fatal("skip_depth not audited")
	}
}

func TestDailyAgentCap(t *testing.T) {
	p := guardPipeline(t, "")
	for id := 1; id <= 3; id++ {
		if _, _, err := p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": id}}, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, p, `SELECT count(*) FROM jobs WHERE rule='start'`); n != 2 {
		t.Fatalf("agent jobs = %d, want 2 (cap)", n)
	}
	if count(t, p, `SELECT count(*) FROM audit WHERE event='skip_agent_cap'`) != 1 {
		t.Fatal("skip_agent_cap not audited")
	}
}
