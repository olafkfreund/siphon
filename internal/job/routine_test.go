package job

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/rule"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

type rt struct {
	t     *testing.T
	p     *Pipeline
	dir   string
	log   string
	sleep []time.Duration
}

// newRT builds a pipeline from YAML with two stub scripts: rec.sh <name> [fail]
// appends <name> to the log and prints {"n":1}; flaky.sh fails until it has run twice.
func newRT(t *testing.T, yaml string) *rt {
	t.Helper()
	dir := t.TempDir()
	r := &rt{t: t, dir: dir, log: filepath.Join(dir, "log")}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rec := write("rec.sh", "echo \"$1\" >> "+r.log+"\nif [ \"$2\" = fail ]; then exit 3; fi\necho '{\"n\":1}'\n")
	flaky := write("flaky.sh", "echo x >> "+filepath.Join(dir, "cnt")+"\n[ \"$(wc -l < "+filepath.Join(dir, "cnt")+")\" -ge 2 ] || exit 1\n")
	yaml = strings.NewReplacer("REC", rec, "FLAKY", flaky).Replace(yaml)
	cfg, err := config.Parse([]byte("server: { sandbox: none }\nsources: { s: { type: http, url: 'http://x' } }\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.p = &Pipeline{Cfg: cfg, Store: st, Now: time.Now,
		rnd:   func() float64 { return 0.5 }, // jitter factor exactly 1.0
		sleep: func(_ context.Context, d time.Duration) error { r.sleep = append(r.sleep, d); return nil },
	}
	return r
}

func (r *rt) fire(id int) int64 {
	r.t.Helper()
	_, ids, err := r.p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": id}}, false)
	if err != nil || len(ids) != 1 {
		r.t.Fatalf("fire: %v %v", ids, err)
	}
	return ids[0]
}

func (r *rt) run() {
	r.t.Helper()
	if _, err := r.p.RunQueued(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rt) job(id int64) store.JobDetail {
	j, err := store.GetJob(r.p.Store.DB, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

func (r *rt) logged() string {
	b, _ := os.ReadFile(r.log)
	return strings.Join(strings.Fields(string(b)), " ")
}

const ruleYAML = `
rules:
  - { name: go, source: s, when: "true", on: each, id: "string(event.id)", action: { routine: r } }
`

func TestRoutineContinueOnError(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r:
    steps:
      - { id: a, cmd: [REC, a] }
      - { id: b, cmd: [REC, b, fail], continue_on_error: true }
      - { id: c, cmd: [REC, c], if: 'steps.b.exit == 3 && steps.a.json.n == 1' }
`)
	id := r.fire(1)
	r.run()
	j := r.job(id)
	if j.State != "done" || r.logged() != "a b c" {
		t.Fatalf("%s log=%q out=%s", j.State, r.logged(), j.Output)
	}
	var prog progress
	json.Unmarshal([]byte(j.Output), &prog)
	if prog.Steps["b"].Exit != 3 || prog.Steps["c"].Exit != 0 || string(prog.Steps["a"].JSON) != `{"n":1}` || prog.Awaiting != nil {
		t.Fatalf("%+v", prog)
	}
}

func TestRoutineStopsOnFailure(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a, fail] }, { id: b, cmd: [REC, b] } ] }
`)
	id := r.fire(1)
	r.run()
	if j := r.job(id); j.State != "failed" || *j.ExitCode != 3 || r.logged() != "a" {
		t.Fatalf("%+v log=%q", j, r.logged())
	}
}

func TestRoutineIfSkipAndTemplatedStepOutput(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r:
    steps:
      - { id: a, cmd: [REC, a] }
      - { id: skipme, cmd: [REC, never], if: 'steps.a.exit != 0' }
      - { id: c, cmd: [REC, "after-{{.steps.a.exit}}-{{.event.id}}"], if: 'steps.skipme.exit == -1' }
`)
	id := r.fire(7)
	r.run()
	j := r.job(id)
	if j.State != "done" || r.logged() != "a after-0-7" || !strings.Contains(j.Output, `"skipped":true`) {
		t.Fatalf("%s log=%q out=%s", j.State, r.logged(), j.Output)
	}
}

func TestRoutineApprovalResumesWithoutRerunning(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r:
    steps:
      - { id: a, cmd: [REC, a] }
      - { id: b, cmd: [REC, b], approve: true }
      - { id: c, cmd: [REC, c] }
      - { id: d, cmd: [REC, d], approve: true }
      - { id: e, cmd: [REC, e] }
`)
	id := r.fire(1)
	r.run()
	j := r.job(id)
	if j.State != "pending_approval" || r.logged() != "a" {
		t.Fatalf("first pause: %s log=%q", j.State, r.logged())
	}
	var resume int
	r.p.Store.DB.QueryRow(`SELECT resume_step FROM jobs WHERE id=?`, id).Scan(&resume)
	if resume != 1 || !strings.Contains(j.Output, `"awaiting":1`) {
		t.Fatalf("resume=%d out=%s", resume, j.Output)
	}
	pend, _ := store.PendingApprovals(r.p.Store.DB)
	if len(pend) != 1 || pend[0].JobID != id {
		t.Fatalf("%+v", pend)
	}

	if err := r.p.Decide(id, true, "olaf"); err != nil {
		t.Fatal(err)
	}
	r.run()
	if j := r.job(id); j.State != "pending_approval" || r.logged() != "a b c" {
		t.Fatalf("second pause: %s log=%q", j.State, r.logged())
	}
	if err := r.p.Decide(id, true, "olaf"); err != nil {
		t.Fatalf("second approval of the same job: %v", err)
	}
	r.run()
	j = r.job(id)
	if j.State != "done" || r.logged() != "a b c d e" {
		t.Fatalf("%s log=%q", j.State, r.logged())
	}
	if err := r.p.Decide(id, true, "olaf"); err == nil {
		t.Fatal("decision on a finished job must fail")
	}
}

func TestRoutineApprovalDenied(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a], approve: true }, { id: b, cmd: [REC, b] } ] }
`)
	id := r.fire(1)
	r.run()
	r.p.Decide(id, false, "olaf")
	r.run()
	if j := r.job(id); j.State != "cancelled" || r.logged() != "" {
		t.Fatalf("%s log=%q", j.State, r.logged())
	}
}

func TestRoutineResumeAfterRestartSkipsDoneSteps(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a] }, { id: b, cmd: [REC, b] } ] }
`)
	id := r.fire(1)
	// Simulate a crash after step a: progress saved, job left running, then startup requeue.
	r.p.Store.DB.Exec(`UPDATE jobs SET state='running', resume_step=1,
		output='{"steps":{"a":{"exit":0,"output":""}}}' WHERE id=?`, id)
	if err := r.p.Requeue(); err != nil {
		t.Fatal(err)
	}
	r.run()
	if j := r.job(id); j.State != "done" || r.logged() != "b" {
		t.Fatalf("%s log=%q", j.State, r.logged())
	}
}

func TestRetryBackoffDurations(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a, fail], retry: { attempts: 4 } } ] }
`)
	id := r.fire(1)
	r.run()
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second}
	if len(r.sleep) != 3 || r.sleep[0] != want[0] || r.sleep[1] != want[1] || r.sleep[2] != want[2] {
		t.Fatalf("sleeps: %v", r.sleep)
	}
	if j := r.job(id); j.State != "failed" || r.logged() != "a a a a" {
		t.Fatalf("%s log=%q", j.State, r.logged())
	}
}

func TestRetryCustomBaseFactorAndJitterBounds(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a, fail], retry: { attempts: 3, base: 1s, factor: 3 } } ] }
`)
	r.fire(1)
	r.run()
	if len(r.sleep) != 2 || r.sleep[0] != time.Second || r.sleep[1] != 3*time.Second {
		t.Fatalf("sleeps: %v", r.sleep)
	}
	for _, c := range []struct {
		rnd  float64
		want time.Duration
	}{{0, 8 * time.Second}, {0.999999, 12 * time.Second}} {
		r.p.rnd = func() float64 { return c.rnd }
		got := r.p.backoff(10*time.Second, 2, 0)
		if d := got - c.want; d < -time.Millisecond || d > time.Millisecond {
			t.Fatalf("rnd=%v: %v want ~%v", c.rnd, got, c.want)
		}
	}
	if r.p.backoff(10*time.Second, 2, 50) > 72*time.Minute { // 1h cap + 20% jitter
		t.Fatal("backoff not capped")
	}
}

func TestRetrySucceedsOnSecondTry(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [FLAKY], retry: { attempts: 3 } }, { id: b, cmd: [REC, b] } ] }
`)
	id := r.fire(1)
	r.run()
	if j := r.job(id); j.State != "done" || len(r.sleep) != 1 || r.logged() != "b" {
		t.Fatalf("%s sleeps=%v log=%q", j.State, r.sleep, r.logged())
	}
}

func TestUnitAllowlistRechecked(t *testing.T) {
	var started []string
	old := runUnit
	runUnit = func(_ context.Context, _ *Pipeline, u string) (int, string) {
		started = append(started, u)
		return 0, "started " + u
	}
	defer func() { runUnit = old }()

	r := newRT(t, `
rules:
  - { name: ok, source: s, when: "true", on: each, id: "string(event.id)", action: { unit: nix-gc.service } }
  - { name: evil, source: s, when: "true", on: each, id: "string(event.id)", action: { unit: evil.service } }
  - { name: rtn, source: s, when: "true", on: each, id: "string(event.id)", action: { routine: r } }
routines:
  r: { steps: [ { id: u, unit: nix-gc.service }, { id: v, unit: evil.service, continue_on_error: true } ] }
units: [nix-gc.service]
`)
	_, ids, err := r.p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": 1}}, false)
	if err != nil || len(ids) != 3 {
		t.Fatal(ids, err)
	}
	r.run()
	if strings.Join(started, ",") != "nix-gc.service,nix-gc.service" {
		t.Fatalf("started: %v", started)
	}
	if j := r.job(ids[0]); j.State != "done" || j.Output != "started nix-gc.service" {
		t.Fatalf("%+v", j)
	}
	j := r.job(ids[1])
	if j.State != "failed" || !strings.Contains(j.Output, "not allowlisted") || *j.ExitCode != -1 {
		t.Fatalf("%+v", j)
	}
	j = r.job(ids[2])
	if j.State != "done" || !strings.Contains(j.Output, "not allowlisted") {
		t.Fatalf("routine: %+v", j)
	}
}

func TestUnitDefaultCallsActionRunUnit(t *testing.T) {
	// The real RunUnit allowlist-checks again; an unlisted unit never reaches systemd.
	r := newRT(t, `
rules:
  - { name: bad, source: s, when: "true", action: { unit: evil.service } }
units: [nix-gc.service]
`)
	r.p.Cfg.Units = []string{"nix-gc.service"}
	// bypass Pipeline.unit's own check by calling the default directly
	exit, out := runUnit(context.Background(), r.p, "evil.service")
	if exit != -1 || !strings.Contains(out, "allowlist") {
		t.Fatalf("%d %q", exit, out)
	}
}

func TestRoutineWithAgentCountsTowardDailyCap(t *testing.T) {
	r := newRT(t, `
limits: { agent_runs_per_day: 1 }
agents: { fix: { prompt: hi, approve: false } }
rules:
  - { name: go, source: s, when: "true", on: each, id: "string(event.id)", action: { routine: r } }
routines:
  r: { steps: [ { id: a, cmd: [REC, a] }, { id: b, agent: fix } ] }
`)
	r.p.Cfg.Limits.AgentRunsPerDay = 1
	r.fire(1)
	_, ids, err := r.p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": 2}}, false)
	if err != nil || len(ids) != 0 {
		t.Fatalf("second routine must be skipped by the cap: %v %v", ids, err)
	}
	var n int
	r.p.Store.DB.QueryRow(`SELECT COUNT(*) FROM audit WHERE event='skip_agent_cap'`).Scan(&n)
	if n != 1 {
		t.Fatal("cap skip not audited")
	}
}

func TestRoutineWithoutAgentIsNotCapped(t *testing.T) {
	r := newRT(t, ruleYAML+`
limits: { agent_runs_per_day: 1 }
routines:
  r: { steps: [ { id: a, cmd: [REC, a] } ] }
`)
	r.p.Cfg.Limits.AgentRunsPerDay = 1
	r.fire(1)
	r.fire(2)
	r.fire(3)
}
