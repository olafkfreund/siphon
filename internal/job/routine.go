package job

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

const (
	statePaused        = "paused"   // run result only: the routine is pending_approval, don't finish it
	stateRequeued      = "requeued" // run result only: a step retry was scheduled, don't finish it
	defaultStepTimeout = 10 * time.Minute
	defaultRetryBase   = 10 * time.Second
	defaultRetryFactor = 2.0
	maxBackoff         = time.Hour
)

// stepResult is steps.<id> in `if` expressions and templates.
type stepResult struct {
	Exit    int             `json:"exit"`
	Output  string          `json:"output"`
	JSON    json.RawMessage `json:"json,omitempty"` // decoded stdout, when it is valid JSON
	Skipped bool            `json:"skipped,omitempty"`
}

// progress is the job's stored output and the resume state.
// Awaiting/AwaitingID name the step that was approved (or is awaiting approval,
// or is being retried), so on resume that step skips the approval gate.
// Tries is how many attempts the current step has used.
type progress struct {
	Steps      map[string]stepResult `json:"steps"`
	Awaiting   *int                  `json:"awaiting,omitempty"`
	AwaitingID string                `json:"awaiting_id,omitempty"`
	Tries      int                   `json:"tries,omitempty"`
}

// stepNeedsApproval: a step gates on its own approve flag, and an agent step
// also on the agent's approve (default true), like an agent rule action does.
func (p *Pipeline) stepNeedsApproval(st config.Step, agents map[string]config.Agent) bool {
	if st.Approve {
		return true
	}
	if st.Agent == "" {
		return false
	}
	a := p.agentDef(st.Agent, agents)
	return a == nil || a.Approve == nil || *a.Approve
}

// runRoutine runs the job's snapshot of the routine's steps from j.ResumeStep
// (the live config is never consulted, so a reload during a pause cannot shift
// indices). Progress is saved after every step, so a restart or an approval
// resumes without re-running finished steps (at-least-once for the step that
// was interrupted). A failed step with retries left re-queues the job with a
// backoff instead of sleeping in the worker.
func (p *Pipeline) runRoutine(ctx context.Context, j store.QueuedJob, pl Payload) (string, int, string) {
	steps := pl.Steps
	if len(steps) == 0 {
		return "failed", -1, "routine snapshot missing from job " + pl.Action.Routine
	}
	prog := progress{Steps: map[string]stepResult{}}
	if j.Output != "" {
		if err := json.Unmarshal([]byte(j.Output), &prog); err != nil || prog.Steps == nil {
			return "failed", -1, "bad routine progress"
		}
	}
	approved := -1
	if a := prog.Awaiting; a != nil {
		if *a != j.ResumeStep || *a >= len(steps) || steps[*a].ID != prog.AwaitingID {
			return "failed", -1, "routine progress does not match the job's steps"
		}
		approved = *a
	}
	prog.Awaiting, prog.AwaitingID = nil, ""
	tries := prog.Tries
	prog.Tries = 0

	for i := j.ResumeStep; i < len(steps); i++ {
		st := steps[i]
		used := tries // attempts already used by this step (only the first pass can have any)
		tries = 0
		env := stepEnv(pl.Env, prog.Steps)
		if st.If != "" {
			run, err := evalIf(ctx, st.If, env)
			if err != nil {
				prog.Steps[st.ID] = stepResult{Exit: -1, Output: "if: " + err.Error()}
				if err := store.SaveProgress(p.Store.DB, j.ID, i+1, marshalProgress(prog)); err != nil {
					return "failed", -1, err.Error()
				}
				if !st.ContinueOnError {
					return "failed", -1, marshalProgress(prog)
				}
				continue
			}
			if !run {
				prog.Steps[st.ID] = stepResult{Exit: -1, Skipped: true}
				if err := store.SaveProgress(p.Store.DB, j.ID, i+1, marshalProgress(prog)); err != nil {
					return "failed", -1, err.Error()
				}
				continue
			}
		}
		if p.stepNeedsApproval(st, pl.Agents) && i != approved {
			return p.pauseRoutine(j, i, st.ID, prog)
		}
		res := p.execOnce(ctx, j, st, env, pl.Agents)
		if ctx.Err() != nil && res.Exit == -1 {
			// Interrupted: don't record it; the job stays running for the startup requeue.
			return "failed", -1, marshalProgress(prog)
		}
		if res.Exit != 0 {
			attempts, base, factor := retryParams(st)
			if used++; used < attempts {
				prog.Tries = used
				prog.Awaiting, prog.AwaitingID = &i, st.ID // an approved step stays approved across retries
				at := p.Now().Add(p.backoff(base, factor, used-1))
				if err := store.RequeueJob(p.Store.DB, j.ID, i, marshalProgress(prog), at); err != nil {
					return "failed", -1, err.Error()
				}
				return stateRequeued, 0, ""
			}
		}
		prog.Steps[st.ID] = res
		if err := store.SaveProgress(p.Store.DB, j.ID, i+1, marshalProgress(prog)); err != nil {
			return "failed", -1, err.Error()
		}
		if res.Exit != 0 && !st.ContinueOnError {
			return "failed", res.Exit, marshalProgress(prog)
		}
	}
	return "done", 0, marshalProgress(prog)
}

func marshalProgress(p progress) string {
	b, _ := json.Marshal(p)
	return string(b)
}

// pauseRoutine parks the job as pending_approval at step i; the job and its new
// approvals row change in one transaction.
func (p *Pipeline) pauseRoutine(j store.QueuedJob, i int, id string, prog progress) (string, int, string) {
	prog.Awaiting, prog.AwaitingID = &i, id
	out := marshalProgress(prog)
	tx, err := p.Store.DB.Begin()
	if err != nil {
		return "failed", -1, err.Error()
	}
	defer tx.Rollback()
	if err := store.PauseJob(tx, j.ID, i, out); err != nil {
		return "failed", -1, err.Error()
	}
	if _, err := p.newApproval(tx, j.ID, p.Now()); err != nil {
		return "failed", -1, err.Error()
	}
	if err := tx.Commit(); err != nil {
		return "failed", -1, err.Error()
	}
	slog.Info("approval required", "job", j.ID, "step", i, "approve", fmt.Sprintf("siphon approve %d", j.ID))
	return statePaused, 0, out
}

// stepEnv is the template/expr env: the event env plus steps.<id>.{exit,output,json,skipped}.
func stepEnv(base map[string]any, steps map[string]stepResult) map[string]any {
	env := make(map[string]any, len(base)+1)
	for k, v := range base {
		env[k] = v
	}
	m := make(map[string]any, len(steps))
	for id, r := range steps {
		e := map[string]any{"exit": r.Exit, "output": r.Output, "json": nil, "skipped": r.Skipped}
		if len(r.JSON) > 0 {
			if v, err := source.DecodeJSON(r.JSON); err == nil {
				e["json"] = v
			}
		}
		m[id] = e
	}
	env["steps"] = m
	return env
}

// evalIf uses the rules' expression cache and 100 ms timeout.
func evalIf(ctx context.Context, src string, env map[string]any) (bool, error) {
	v, err := rule.Eval(ctx, src, env)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("returned %T, want bool", v)
	}
	return b, nil
}

// retryParams: total tries and backoff settings (agent steps never retry).
func retryParams(st config.Step) (attempts int, base time.Duration, factor float64) {
	attempts, base, factor = 1, defaultRetryBase, defaultRetryFactor
	if r := st.Retry; r != nil && st.Agent == "" {
		attempts = max(attempts, r.Attempts)
		if r.Base > 0 {
			base = time.Duration(r.Base)
		}
		if r.Factor >= 1 {
			factor = r.Factor
		}
	}
	return
}

// backoff is base*factor^n capped at maxBackoff, times a jitter factor in [0.8, 1.2).
func (p *Pipeline) backoff(base time.Duration, factor float64, n int) time.Duration {
	d := float64(base)
	for i := 0; i < n && d < float64(maxBackoff); i++ {
		d *= factor
	}
	d = min(d, float64(maxBackoff))
	r := rand.Float64
	if p.rnd != nil {
		r = p.rnd
	}
	return time.Duration(d * (0.8 + 0.4*r()))
}

func (p *Pipeline) execOnce(ctx context.Context, j store.QueuedJob, st config.Step, env map[string]any, agents map[string]config.Agent) stepResult {
	timeout := time.Duration(st.Timeout)
	if timeout <= 0 {
		timeout = defaultStepTimeout
	}
	var exit int
	var out string
	var stdout []byte
	switch {
	case len(st.Cmd) > 0:
		argv, err := action.Render(st.Cmd, env)
		if err != nil {
			return stepResult{Exit: -1, Output: err.Error()}
		}
		opts := p.sandbox(timeout)
		allow, on := p.ruleEgress(j.Rule) // cmd steps follow their rule's egress
		egEnv, finish, eerr := p.egressFor(j.ID, allow, on)
		if eerr != nil {
			return stepResult{Exit: -1, Output: eerr.Error()}
		}
		opts.Egress = egEnv
		code, o, so, err := action.RunCmdSplit(ctx, argv, opts, p.Cfg.Secrets())
		exit, out, stdout = code, string(o)+finish(), so
		if err != nil {
			out += err.Error()
		}
	case st.Unit != "":
		uctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		exit, out = runUnit(uctx, p, st.Unit)
	case st.Agent != "":
		actx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var state string
		state, exit, out, stdout = p.agentExec(actx, j, Payload{Action: config.Action{Agent: st.Agent}, Env: env, Agents: agents}, true)
		if state != "done" && exit == 0 {
			exit = -1
		}
	default:
		return stepResult{Exit: -1, Output: "step has no action"}
	}
	res := stepResult{Exit: exit, Output: out}
	if t := bytes.TrimSpace(stdout); len(t) > 0 && json.Valid(t) {
		var c bytes.Buffer
		if json.Compact(&c, t) == nil {
			res.JSON = c.Bytes()
		}
	}
	return res
}
