package job

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/olafkfreund/MCP-AgentGateway/internal/action"
	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/source"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

const (
	statePaused        = "paused" // run result only: the routine is pending_approval, don't finish it
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

// progress is the job's stored output and the resume state. Awaiting is the
// index of the step whose approval is pending; once approved, that step skips
// the gate on resume.
type progress struct {
	Steps    map[string]stepResult `json:"steps"`
	Awaiting *int                  `json:"awaiting,omitempty"`
}

func (p *Pipeline) routineHasAgent(name string) bool {
	if rt := p.Cfg.Routines[name]; rt != nil {
		for _, s := range rt.Steps {
			if s.Agent != "" {
				return true
			}
		}
	}
	return false
}

// runRoutine runs steps in order from j.ResumeStep. Progress is saved after
// every step, so a restart or an approval resumes without re-running finished
// steps (at-least-once for the step that was interrupted).
func (p *Pipeline) runRoutine(ctx context.Context, j store.QueuedJob, pl Payload) (string, int, string) {
	rt := p.Cfg.Routines[pl.Action.Routine]
	if rt == nil || len(rt.Steps) == 0 {
		return "failed", -1, "unknown or empty routine " + pl.Action.Routine
	}
	prog := progress{Steps: map[string]stepResult{}}
	if j.Output != "" {
		if err := json.Unmarshal([]byte(j.Output), &prog); err != nil || prog.Steps == nil {
			return "failed", -1, "bad routine progress"
		}
	}
	approved := -1
	if prog.Awaiting != nil {
		approved = *prog.Awaiting
	}
	prog.Awaiting = nil

	for i := j.ResumeStep; i < len(rt.Steps); i++ {
		st := rt.Steps[i]
		env := stepEnv(pl.Env, prog.Steps)
		if st.If != "" {
			run, err := evalIf(st.If, env)
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
		if st.Approve && i != approved {
			return p.pauseRoutine(j, i, prog)
		}
		res := p.execStep(ctx, j, st, env)
		if ctx.Err() != nil && res.Exit == -1 {
			// Interrupted: don't record it; the job stays running for the startup requeue.
			return "failed", -1, marshalProgress(prog)
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
func (p *Pipeline) pauseRoutine(j store.QueuedJob, i int, prog progress) (string, int, string) {
	prog.Awaiting = &i
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
	slog.Info("approval required", "job", j.ID, "step", i, "approve", fmt.Sprintf("agentgw approve %d", j.ID))
	return statePaused, 0, out
}

// stepEnv is the template/expr env: the event env plus steps.<id>.{exit,output,json}.
func stepEnv(base map[string]any, steps map[string]stepResult) map[string]any {
	env := make(map[string]any, len(base)+1)
	for k, v := range base {
		env[k] = v
	}
	m := make(map[string]any, len(steps))
	for id, r := range steps {
		e := map[string]any{"exit": r.Exit, "output": r.Output, "json": nil}
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

func evalIf(src string, env map[string]any) (bool, error) {
	prog, err := expr.Compile(src, expr.Env(map[string]any{}), expr.AllowUndefinedVariables())
	if err != nil {
		return false, err
	}
	v, err := vm.Run(prog, env)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("returned %T, want bool", v)
	}
	return b, nil
}

// execStep runs a step, retrying failures with exponential backoff and jitter.
func (p *Pipeline) execStep(ctx context.Context, j store.QueuedJob, st config.Step, env map[string]any) stepResult {
	attempts, base, factor := 1, defaultRetryBase, defaultRetryFactor
	if r := st.Retry; r != nil {
		attempts = max(attempts, r.Attempts)
		if r.Base > 0 {
			base = time.Duration(r.Base)
		}
		if r.Factor >= 1 {
			factor = r.Factor
		}
	}
	var res stepResult
	for n := 0; n < attempts; n++ {
		if n > 0 {
			if err := p.sleepFn(ctx, p.backoff(base, factor, n-1)); err != nil {
				return stepResult{Exit: -1, Output: res.Output}
			}
		}
		res = p.execOnce(ctx, j, st, env)
		if res.Exit == 0 || ctx.Err() != nil {
			break
		}
	}
	return res
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

func (p *Pipeline) sleepFn(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *Pipeline) execOnce(ctx context.Context, j store.QueuedJob, st config.Step, env map[string]any) stepResult {
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
		code, o, so, err := action.RunCmdSplit(ctx, argv,
			action.SandboxOptions{Mode: p.Cfg.Server.Sandbox, Timeout: timeout}, p.Cfg.Secrets())
		exit, out, stdout = code, string(o), so
		if err != nil {
			out += err.Error()
		}
	case st.Unit != "":
		uctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		exit, out = p.unit(uctx, st.Unit)
	case st.Agent != "":
		actx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var state string
		state, exit, out, stdout = p.agentExec(actx, j, Payload{Action: config.Action{Agent: st.Agent}, Env: env})
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
