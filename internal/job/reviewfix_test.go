package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

// #10 item 3: if leftover action units can't be stopped, refuse to start
// rather than requeue jobs that may still be running.
func TestOrphanStopFailureRefusesStart(t *testing.T) {
	p, _ := subPipeline(t, "cat >/dev/null; echo ok\n")
	p.Config().Server.Sandbox = "systemd"
	orig := stopOrphans
	stopOrphans = func(context.Context) error { return errors.New("polkit said no") }
	t.Cleanup(func() { stopOrphans = orig })

	tx, _ := p.Store.DB.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "start", ActionJSON: "{}"}, time.Now())
	tx.Commit()
	p.Store.DB.Exec(`UPDATE jobs SET state='running' WHERE id=?`, id)

	if err := p.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("RunOnce err = %v, want refusal", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Serve(ctx); err == nil {
		t.Fatal("Serve started despite the orphan-stop failure")
	}
	var state string
	p.Store.DB.QueryRow(`SELECT state FROM jobs WHERE id=?`, id).Scan(&state)
	if state != "running" {
		t.Fatalf("job was requeued (%s) although its unit may still run", state)
	}
}

// #10 item 4: when the agent-result event can't be committed, the job fails
// instead of reporting done with the result silently lost.
func TestAgentResultCommitFailureFailsJob(t *testing.T) {
	p, _ := subPipeline(t, "cat >/dev/null; echo answer\n")
	p.Config().Rules = append(p.Config().Rules, config.Rule{Name: "follow", Source: config.AgentResultSource,
		AllowAgentEvents: true, When: "true", Action: config.Action{Cmd: []string{"echo", "x"}}})
	if _, err := p.Store.DB.Exec(`CREATE TRIGGER boom BEFORE INSERT ON jobs WHEN NEW.rule='follow'
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	state, out := runAgentJob(t, p, 50)
	if state != "failed" || !strings.Contains(out, "agent result not recorded") {
		t.Fatalf("state %s output %q, want failed + not recorded", state, out)
	}
}

// #10 item 5: a queued job runs the agent definition it was enqueued with,
// not one edited afterwards.
func TestQueuedJobUsesSnapshottedAgent(t *testing.T) {
	p, _ := subPipeline(t, "cat\n") // the stub echoes the prompt it got on stdin
	if _, _, err := p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": 60}}, false); err != nil {
		t.Fatal(err)
	}
	p.Config().Agents["fix"].Prompt = "EDITED after enqueue" // operator edits config while the job waits
	if _, err := p.RunQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out string
	p.Store.DB.QueryRow(`SELECT output FROM jobs WHERE rule='start'`).Scan(&out)
	if !strings.Contains(out, "fix 60") || strings.Contains(out, "EDITED") {
		t.Fatalf("job ran with %q, want the snapshotted prompt", out)
	}
}

// #10 item 1 caller: a write-back that lost to a re-import is logged as stale.
func TestStaleWritebackAudited(t *testing.T) {
	newAuth := codexAuth(t, "refresh-from-run", time.Now().Add(48*time.Hour))
	p, cs := subPipeline(t, "cat >/dev/null; sleep 1; printf '%s' '"+string(newAuth)+"' > \"$CODEX_HOME/auth.json\"; echo done\n")
	go func() {
		time.Sleep(300 * time.Millisecond) // operator re-imports while the run is going
		cs.Put("chatgpt", "auth.json", codexAuth(t, "reimported", time.Now().Add(time.Hour)))
	}()
	if state, out := runAgentJob(t, p, 70); state != "done" {
		t.Fatalf("state %s: %s", state, out)
	}
	files, _ := cs.Load("chatgpt")
	if !strings.Contains(string(files["auth.json"]), "reimported") {
		t.Fatalf("stale run overwrote the re-import: %s", files["auth.json"])
	}
	if count(t, p, `SELECT count(*) FROM audit WHERE event='credential_writeback_stale'`) != 1 {
		t.Fatal("stale write-back not audited")
	}
}
