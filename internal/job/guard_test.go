package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
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
	if n := count(t, p, `SELECT count(*) FROM jobs WHERE rule='follows-agents' AND depth=1 AND parent_id IS NOT NULL AND output LIKE 'followed%'`); n != 1 {
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

// Phase 1 review M1: a job that completed while cancellation arrived is
// recorded (never re-run); one killed by cancellation (exit -1) stays running.
func TestCancellationFinishSemantics(t *testing.T) {
	for _, tc := range []struct {
		exit int
		want string
	}{{0, "done"}, {-1, "running"}} {
		p := guardPipeline(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		tx, _ := p.Store.DB.Begin()
		if _, err := store.InsertJob(tx, store.Job{Rule: "x", ActionJSON: "{}"}, p.Now()); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		p.runFn = func(context.Context, store.QueuedJob) (string, int, string) {
			cancel() // cancellation lands while the action runs
			if tc.exit == 0 {
				return "done", 0, "ok"
			}
			return "failed", -1, "killed"
		}
		p.runOne(ctx)
		var state string
		p.Store.DB.QueryRow(`SELECT state FROM jobs`).Scan(&state)
		if state != tc.want {
			t.Fatalf("exit %d: state %s, want %s", tc.exit, state, tc.want)
		}
	}
}

// Phase 1 review L6 / plan step 9: the agent's MCP config holds only the
// sources listed in agents.<name>.mcp, with bearer auth attached.
func TestAgentGetsOnlyListedMCPSources(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "cat-config.sh")
	// Print the --mcp-config file so the job output shows what the agent got.
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --mcp-config ] && cat \"$2\"; shift; done\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGW_M1_TOKEN", "tok-m1")
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
sources:
  m1: { type: mcp, url: https://m1.example/mcp, read: { resource: "x://a" }, auth: { bearer: env:AGW_M1_TOKEN } }
  m2: { type: mcp, url: https://m2.example/mcp, read: { resource: "x://b" } }
agents:
  a: { runner: [` + stub + `], prompt: "p", mcp: [m1], approve: false }
rules:
  - { name: r, source: m2, when: 'true', cooldown: 1s, action: { agent: a } }
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
	ctx := context.Background()
	if _, _, err := p.HandleEvent(ctx, rule.Event{Source: "m2", Data: map[string]any{}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RunQueued(ctx); err != nil {
		t.Fatal(err)
	}
	var out string
	if err := st.DB.QueryRow(`SELECT output FROM jobs WHERE rule='r'`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "m1.example") || strings.Contains(out, "m2.example") {
		t.Fatalf("agent MCP config = %s; want only m1", out)
	}
	if strings.Contains(out, "tok-m1") || !strings.Contains(out, "Bearer ***") {
		t.Fatalf("bearer not attached or not masked in output: %s", out)
	}
}
