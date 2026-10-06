package job

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

type rt struct {
	t   *testing.T
	p   *Pipeline
	dir string
	log string
	now time.Time // injected clock
}

// newRT builds a pipeline from YAML with two stub scripts: rec.sh <name> [fail]
// appends <name> to the log and prints {"n":1}; flaky.sh fails until it has run twice.
func newRT(t *testing.T, yaml string) *rt {
	t.Helper()
	dir := t.TempDir()
	r := &rt{t: t, dir: dir, log: filepath.Join(dir, "log"), now: time.Unix(1_800_000_000, 0)}
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
	r.p = &Pipeline{Cfg: cfg, Store: st, Now: func() time.Time { return r.now },
		rnd: func() float64 { return 0.5 }, // jitter factor exactly 1.0
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

func (r *rt) runAfter(id int64) time.Time {
	var ms int64
	r.p.Store.DB.QueryRow(`SELECT run_after FROM jobs WHERE id=?`, id).Scan(&ms)
	return time.UnixMilli(ms)
}

func TestRetryBackoffRequeuesInsteadOfSleeping(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a, fail], retry: { attempts: 4 } } ] }
`)
	id := r.fire(1)
	for i, wait := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		r.run()
		j := r.job(id)
		if j.State != "queued" || r.logged() != strings.Repeat("a ", i+1)[:2*i+1] {
			t.Fatalf("attempt %d: %s log=%q", i+1, j.State, r.logged())
		}
		if got := r.runAfter(id); !got.Equal(r.now.Add(wait)) {
			t.Fatalf("attempt %d: run_after %v want now+%v", i+1, got, wait)
		}
		if !strings.Contains(j.Output, `"tries":`+itoaT(i+1)) {
			t.Fatalf("tries not persisted: %s", j.Output)
		}
		r.run() // before run_after nothing may run
		if n := strings.Count(r.logged(), "a"); n != i+1 {
			t.Fatalf("ran again before its backoff: %d", n)
		}
		r.now = r.now.Add(wait)
	}
	r.run()
	j := r.job(id)
	if j.State != "failed" || *j.ExitCode != 3 || r.logged() != "a a a a" {
		t.Fatalf("%s log=%q", j.State, r.logged())
	}
}

func itoaT(n int) string { return string(rune('0' + n)) }

func TestRetryCustomBaseFactorAndJitterBounds(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a, fail], retry: { attempts: 3, base: 1s, factor: 3 } } ] }
`)
	id := r.fire(1)
	r.run()
	if got := r.runAfter(id); !got.Equal(r.now.Add(time.Second)) {
		t.Fatalf("first wait: %v", got.Sub(r.now))
	}
	r.now = r.now.Add(time.Second)
	r.run()
	if got := r.runAfter(id); !got.Equal(r.now.Add(3 * time.Second)) {
		t.Fatalf("second wait: %v", got.Sub(r.now))
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
	if r.job(id).State != "queued" {
		t.Fatal("first failure should requeue")
	}
	r.now = r.now.Add(11 * time.Second)
	r.run()
	if j := r.job(id); j.State != "done" || r.logged() != "b" || strings.Contains(j.Output, "tries") {
		t.Fatalf("%s log=%q out=%s", j.State, r.logged(), j.Output)
	}
}

func TestApprovedStepStaysApprovedAcrossRetry(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [FLAKY], approve: true, retry: { attempts: 3 } } ] }
`)
	id := r.fire(1)
	r.run()
	r.p.Decide(id, true, "olaf")
	r.run() // first try fails -> requeued, must not ask for approval again
	if st := r.job(id).State; st != "queued" {
		t.Fatalf("state %s", st)
	}
	r.now = r.now.Add(11 * time.Second)
	r.run()
	if st := r.job(id).State; st != "done" {
		t.Fatalf("state %s", st)
	}
}

func TestUnitStepsAndActionUseRunUnitSeam(t *testing.T) {
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
  - { name: rtn, source: s, when: "true", on: each, id: "string(event.id)", action: { routine: r } }
routines:
  r: { steps: [ { id: u, unit: nix-gc.service } ] }
units: [nix-gc.service]
`)
	_, ids, err := r.p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": 1}}, false)
	if err != nil || len(ids) != 2 {
		t.Fatal(ids, err)
	}
	r.run()
	if strings.Join(started, ",") != "nix-gc.service,nix-gc.service" {
		t.Fatalf("started: %v", started)
	}
	if j := r.job(ids[0]); j.State != "done" || j.Output != "started nix-gc.service" {
		t.Fatalf("%+v", j)
	}
}

func TestUnitDefaultRejectsUnlistedUnit(t *testing.T) {
	// The real runUnit (action.RunUnit) enforces the allowlist; nothing reaches systemd.
	r := newRT(t, `
rules:
  - { name: bad, source: s, when: "true", action: { unit: evil.service } }
  - { name: rtn, source: s, when: "true", action: { routine: r } }
routines:
  r: { steps: [ { id: v, unit: evil.service, continue_on_error: true } ] }
units: [nix-gc.service]
`)
	_, ids, _ := r.p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": 1}}, false)
	r.run()
	j := r.job(ids[0])
	if j.State != "failed" || *j.ExitCode != -1 || !strings.Contains(j.Output, "allowlist") {
		t.Fatalf("%+v", j)
	}
	if j := r.job(ids[1]); j.State != "done" || !strings.Contains(j.Output, "allowlist") {
		t.Fatalf("routine: %+v", j)
	}
}

func TestAgentStepHonoursAgentApprove(t *testing.T) {
	r := newRT(t, ruleYAML+`
agents:
  fix: { prompt: hi }
  quiet: { prompt: hi, approve: false }
routines:
  r: { steps: [ { id: a, cmd: [REC, a] }, { id: b, agent: fix } ] }
`)
	id := r.fire(1)
	r.run()
	j := r.job(id)
	if j.State != "pending_approval" || r.logged() != "a" || !strings.Contains(j.Output, `"awaiting":1`) {
		t.Fatalf("agent step must pause for approval: %s log=%q out=%s", j.State, r.logged(), j.Output)
	}
	// An agent with approve: false and no step-level approve does not gate.
	if r.p.stepNeedsApproval(config.Step{Agent: "quiet"}, nil) || !r.p.stepNeedsApproval(config.Step{Agent: "fix"}, nil) ||
		!r.p.stepNeedsApproval(config.Step{Agent: "quiet", Approve: true}, nil) || r.p.stepNeedsApproval(config.Step{Cmd: []string{"x"}}, nil) {
		t.Fatal("stepNeedsApproval")
	}
}

func TestRoutineUsesSnapshotNotLiveConfig(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a] }, { id: b, cmd: [REC, b], approve: true }, { id: c, cmd: [REC, c] } ] }
`)
	id := r.fire(1)
	r.run()
	// The config changes while the job waits: a step is inserted and approve dropped.
	r.p.Cfg.Routines["r"].Steps = []config.Step{
		{ID: "x", Cmd: []string{"REC-never", "x"}}, {ID: "a", Cmd: []string{"false"}}, {ID: "b", Cmd: []string{"false"}},
	}
	r.p.Decide(id, true, "olaf")
	r.run()
	if j := r.job(id); j.State != "done" || r.logged() != "a b c" {
		t.Fatalf("%s log=%q out=%s", j.State, r.logged(), j.Output)
	}
}

func TestAwaitedStepIDMismatchFails(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a] }, { id: b, cmd: [REC, b], approve: true } ] }
`)
	id := r.fire(1)
	r.run()
	r.p.Store.DB.Exec(`UPDATE jobs SET output=replace(output, '"awaiting_id":"b"', '"awaiting_id":"zzz"') WHERE id=?`, id)
	r.p.Decide(id, true, "olaf")
	r.run()
	if j := r.job(id); j.State != "failed" || !strings.Contains(j.Output, "does not match") || r.logged() != "a" {
		t.Fatalf("%s log=%q out=%s", j.State, r.logged(), j.Output)
	}
}

func TestSkippedExposedInStepEnv(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r:
    steps:
      - { id: s, cmd: [REC, never], if: "false" }
      - { id: t, cmd: [REC, t], if: "steps.s.skipped == true" }
`)
	id := r.fire(1)
	r.run()
	if j := r.job(id); j.State != "done" || r.logged() != "t" {
		t.Fatalf("%s log=%q", j.State, r.logged())
	}
}

func TestExpiryKeepsRoutineProgress(t *testing.T) {
	r := newRT(t, ruleYAML+`
routines:
  r: { steps: [ { id: a, cmd: [REC, a] }, { id: b, cmd: [REC, b], approve: true } ] }
`)
	id := r.fire(1)
	r.run()
	r.now = r.now.Add(approvalTTL + time.Second)
	if err := r.p.ExpireApprovals(); err != nil {
		t.Fatal(err)
	}
	j := r.job(id)
	if j.State != "failed" || !strings.Contains(j.Output, `"steps"`) {
		t.Fatalf("%s out=%s", j.State, j.Output)
	}
	var n int
	r.p.Store.DB.QueryRow(`SELECT COUNT(*) FROM audit WHERE event='approval_expired' AND job_id=?`, id).Scan(&n)
	if n != 1 {
		t.Fatal("expiry must be audited")
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
