// Package job connects sources, rules and actions: poll → evaluate → enqueue → run.
// run-once and serve both go through Pipeline, so they behave identically.
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/MCP-AgentGateway/internal/action"
	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/rule"
	"github.com/olafkfreund/MCP-AgentGateway/internal/source"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

// cmdTimeout bounds a cmd action; ponytail: fixed until a rule needs its own.
const cmdTimeout = 10 * time.Minute

type Pipeline struct {
	Cfg   *config.Config
	Store *store.Store
	Now   func() time.Time
	nudge chan struct{} // set by Serve; wakes idle workers after enqueues
	// MCPTransport, if set, supplies the transport for an mcp source (tests).
	MCPTransport func(source string) mcp.Transport

	runFn func(context.Context, store.QueuedJob) (string, int, string) // tests only
}

// New returns a Pipeline ready for Serve. The worker nudge channel is made
// here, before any HTTP handler can call deliver (no race with Serve).
func New(cfg *config.Config, st *store.Store, now func() time.Time) *Pipeline {
	return &Pipeline{Cfg: cfg, Store: st, Now: now, nudge: make(chan struct{}, 64)}
}

// Payload is what a job row carries: the action and the env it renders with.
type Payload struct {
	Action config.Action  `json:"action"`
	Env    map[string]any `json:"env"`
}

// Poll fetches one event from a polled source.
func (p *Pipeline) Poll(ctx context.Context, name string) (rule.Event, error) {
	s := p.Cfg.Sources[name]
	if s == nil {
		return rule.Event{}, fmt.Errorf("unknown source %q", name)
	}
	var ev source.Event
	var err error
	switch s.Type {
	case "http":
		ev, err = source.HTTP{Options: source.HTTPOptions{
			Name: name, URL: s.URL, Method: s.Method, Headers: headerValues(s.Headers), Body: []byte(s.Body),
			AllowPrivate: s.AllowPrivate, MaxBody: int64(p.Cfg.Limits.HTTPMaxBody), Timeout: time.Duration(p.Cfg.Limits.HTTPTimeout),
		}}.Poll(ctx)
	case "mcp":
		o := p.mcpOptions(name)
		m := source.MCP{Options: o}
		if p.MCPTransport != nil {
			m.Transport = p.MCPTransport(name)
		}
		ev, err = m.Poll(ctx)
	default:
		return rule.Event{}, fmt.Errorf("source %q of type %s is not polled", name, s.Type)
	}
	if err != nil {
		return rule.Event{}, err
	}
	return rule.Event{Source: name, Headers: ev.Headers, Data: ev.Data}, nil
}

// Tick polls one source and enqueues jobs for every rule that fires.
func (p *Pipeline) Tick(ctx context.Context, name string) ([]int64, error) {
	ev, err := p.Poll(ctx, name)
	msg := ""
	if err != nil {
		msg = string(action.Mask([]byte(err.Error()), p.Cfg.Secrets()))
	}
	if serr := store.PutSourceState(p.Store.DB, name, p.Now(), msg); serr != nil {
		return nil, serr
	}
	if err != nil {
		return nil, err
	}
	_, ids, err := p.HandleEvent(ctx, ev, false)
	return ids, err
}

// HandleEvent evaluates every rule against ev in one transaction and, unless
// dryRun, enqueues a job per fire. Rule state and job inserts commit together.
func (p *Pipeline) HandleEvent(ctx context.Context, ev rule.Event, dryRun bool) ([]rule.Fire, []int64, error) {
	fires, ids, _, ruleErr, err := p.handleEvent(ctx, ev, dryRun, "", "")
	return fires, ids, errors.Join(err, ruleErr)
}

// handleEvent is HandleEvent plus an optional replay check: when seenScope is
// set, (seenScope, seenID) is recorded in the same transaction as the jobs, and
// dup=true is returned without evaluating anything if it was already seen.
// ruleErr holds per-rule evaluation errors from a transaction that still
// committed; err means nothing was committed.
func (p *Pipeline) handleEvent(ctx context.Context, ev rule.Event, dryRun bool, seenScope, seenID string) (fires []rule.Fire, ids []int64, dup bool, ruleErr, err error) {
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, nil, err
	}
	defer tx.Rollback()
	now := p.Now()
	if seenScope != "" {
		isNew, err := store.MarkSeen(tx, seenScope, seenID, now)
		if err != nil || !isNew {
			return nil, nil, !isNew, nil, err
		}
	}
	var approvalIDs []int64
	var errs []error
	for _, r := range p.Cfg.Rules {
		if on, _, err := store.RuleEnabled(tx, r.Name); err != nil {
			return nil, nil, false, nil, err
		} else if !on {
			continue // disabled at runtime from the portal/API
		}
		fs, err := rule.Evaluate(ctx, tx, r, ev, now, dryRun)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", r.Name, err))
		}
		fires = append(fires, fs...)
		if dryRun {
			continue
		}
		for _, f := range fs {
			id, link, err := p.enqueue(tx, r, f, ev, now)
			if err != nil {
				return nil, nil, false, nil, err
			}
			if id == 0 {
				continue // skipped by the loop guard or the agent cap (audited)
			}
			ids = append(ids, id)
			if link != "" {
				approvalIDs = append(approvalIDs, id)
			}
		}
	}
	if !dryRun {
		if err := tx.Commit(); err != nil {
			return nil, nil, false, nil, err
		}
		for _, id := range approvalIDs { // after commit, so a rolled-back job is never announced
			// ponytail: the one-shot token is a credential and is never logged;
			// operators approve by id in the portal, API or CLI (no token link; plan deviation).
			slog.Info("approval required", "job", id, "approve", fmt.Sprintf("agentgw approve %d", id))
		}
	}
	return fires, ids, false, errors.Join(errs...), nil
}

// enqueue inserts the job; for pending_approval it also creates the approval
// in the same tx and returns its link path.
func (p *Pipeline) enqueue(tx *sql.Tx, r config.Rule, f rule.Fire, ev rule.Event, now time.Time) (id int64, link string, err error) {
	depth := ev.Depth
	if depth > config.MaxDepth {
		return 0, "", store.Audit(tx, now, "rule:"+r.Name, "skip_depth", 0, fmt.Sprintf("depth %d > %d", depth, config.MaxDepth))
	}
	if r.Action.Agent != "" {
		n, err := store.CountAgentJobsSince(tx, now.Add(-24*time.Hour))
		if err != nil {
			return 0, "", err
		}
		if n >= p.Cfg.Limits.AgentRunsPerDay {
			return 0, "", store.Audit(tx, now, "rule:"+r.Name, "skip_agent_cap", 0, fmt.Sprintf("%d agent runs in 24h", n))
		}
	}
	b, err := json.Marshal(Payload{Action: r.Action, Env: f.Env})
	if err != nil {
		return 0, "", err
	}
	state := "queued"
	if p.needsApproval(r) {
		state = "pending_approval"
	}
	id, err = store.InsertJob(tx, store.Job{Rule: r.Name, ActionJSON: string(b), State: state, RunAfter: now, Depth: depth, ParentID: ev.ParentID}, now)
	if err != nil {
		return 0, "", err
	}
	if err := store.Audit(tx, now, "rule:"+r.Name, "fire", id, f.Key); err != nil {
		return 0, "", err
	}
	if state == "pending_approval" {
		link, err = p.newApproval(tx, id, now)
	}
	return id, link, err
}

func (p *Pipeline) needsApproval(r config.Rule) bool {
	if r.Approve {
		return true
	}
	if a := p.Cfg.Agents[r.Action.Agent]; a != nil && a.Approve != nil {
		return *a.Approve
	}
	return false
}

// RunQueued runs runnable jobs one by one until none are left; returns how many ran.
func (p *Pipeline) RunQueued(ctx context.Context) (int, error) {
	n := 0
	for {
		ran, err := p.runOne(ctx)
		if err != nil || !ran {
			return n, err
		}
		n++
	}
}

// runOne claims and runs a single job; ran is false when the queue is empty.
func (p *Pipeline) runOne(ctx context.Context) (ran bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	j, ok, err := store.ClaimJob(p.Store.DB, p.Now())
	if err != nil || !ok {
		return false, err
	}
	exec := p.run
	if p.runFn != nil {
		exec = p.runFn
	}
	state, exit, out := exec(ctx, j)
	if err := ctx.Err(); err != nil && exit == -1 {
		// Killed by cancellation (exit -1 = signalled or never started): leave
		// it running so the startup requeue retries it. A job that completed
		// before cancellation is recorded below, so it never runs twice.
		return false, err
	}
	if err := store.FinishJob(p.Store.DB, j.ID, state, exit, out, p.Now()); err != nil {
		return false, err
	}
	return true, nil
}

func (p *Pipeline) run(ctx context.Context, j store.QueuedJob) (state string, exit int, output string) {
	pl, err := decodePayload(j.ActionJSON)
	if err != nil {
		return "failed", -1, "bad payload: " + err.Error()
	}
	switch {
	case len(pl.Action.Cmd) > 0:
		argv, err := action.Render(pl.Action.Cmd, pl.Env)
		if err != nil {
			return "failed", -1, err.Error()
		}
		code, out, err := action.RunCmd(ctx, argv,
			action.SandboxOptions{Mode: p.Cfg.Server.Sandbox, Timeout: cmdTimeout}, p.Cfg.Secrets())
		if err != nil {
			return "failed", code, string(out) + err.Error()
		}
		if code != 0 {
			return "failed", code, string(out)
		}
		return "done", 0, string(out)
	case pl.Action.Agent != "":
		return p.runAgent(ctx, j, pl)
	default:
		return "failed", -1, "action type not implemented yet (unit/routine arrive in plan phase 3)"
	}
}

// runAgent runs the agent and feeds its JSON result back as an agent-result
// event one level deeper; rules see it only with allow_agent_events (loop guard).
func (p *Pipeline) runAgent(ctx context.Context, j store.QueuedJob, pl Payload) (string, int, string) {
	a := p.Cfg.Agents[pl.Action.Agent]
	if a == nil {
		return "failed", -1, "unknown agent " + pl.Action.Agent
	}
	servers := map[string]action.MCPServer{}
	for _, name := range a.MCP {
		s := p.Cfg.Sources[name]
		h := headerValues(s.Headers)
		if s.Auth != nil && s.Auth.Bearer.Value != "" {
			h["Authorization"] = "Bearer " + s.Auth.Bearer.Value
		}
		servers[name] = action.MCPServer{URL: s.URL, Command: s.Command, Headers: h}
	}
	res, err := action.RunAgent(ctx, action.AgentOptions{
		Runner: a.Runner, Prompt: a.Prompt, Env: pl.Env, MCP: servers,
		AllowedTools: a.AllowedTools, MaxTurns: a.MaxTurns, MaxBudgetUSD: a.MaxBudgetUSD,
		Timeout: time.Duration(a.Timeout), Sandbox: action.SandboxOptions{Mode: p.Cfg.Server.Sandbox},
		Secrets: p.Cfg.Secrets(), WorkDir: filepath.Join(filepath.Dir(p.Cfg.Server.DB), "jobs", strconv.FormatInt(j.ID, 10)),
		APIKeyFile: a.APIKeyFile,
	})
	out := string(res.Output)
	if err != nil {
		return "failed", res.Exit, out + err.Error()
	}
	if res.Exit != 0 {
		return "failed", res.Exit, out
	}
	if data, derr := source.DecodeJSON(res.Stdout); derr == nil {
		ev := rule.Event{Source: config.AgentResultSource, Data: data, Depth: j.Depth + 1, ParentID: j.ID}
		if _, _, herr := p.HandleEvent(ctx, ev, false); herr != nil {
			slog.Warn("agent-result rules", "job", j.ID, "err", herr)
		}
	} else {
		slog.Warn("agent stdout is not JSON; no agent-result event", "job", j.ID, "err", derr)
	}
	return "done", 0, out
}

// RunOnce polls every polled source once, then runs whatever got queued.
func (p *Pipeline) RunOnce(ctx context.Context) error {
	if err := p.Requeue(); err != nil {
		return err
	}
	if err := p.ExpireApprovals(); err != nil {
		return err
	}
	var errs []error
	for _, name := range sortedSources(p.Cfg) {
		if p.Cfg.Sources[name].Type == "webhook" {
			continue
		}
		if _, err := p.Tick(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("source %s: %w", name, err))
		}
	}
	if _, err := p.RunQueued(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func sortedSources(c *config.Config) []string {
	names := make([]string, 0, len(c.Sources))
	for n := range c.Sources {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// headerValues resolves secret-capable header values: refs use the resolved
// Value, inline literals the Ref text.
func headerValues(h map[string]config.Secret) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v.Value
		if out[k] == "" {
			out[k] = v.Ref
		}
	}
	return out
}

// decodePayload keeps integers as int64 so templates render 1700000000, not 1.7e+09.
func decodePayload(s string) (Payload, error) {
	var raw struct {
		Action config.Action   `json:"action"`
		Env    json.RawMessage `json:"env"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return Payload{}, err
	}
	env, err := source.DecodeJSON(raw.Env)
	if err != nil {
		return Payload{}, err
	}
	m, _ := env.(map[string]any)
	return Payload{Action: raw.Action, Env: m}, nil
}

func (p *Pipeline) mcpOptions(name string) source.MCPOptions {
	s := p.Cfg.Sources[name]
	o := source.MCPOptions{
		Name: name, Command: s.Command, URL: s.URL, AllowPrivate: s.AllowPrivate,
		MaxBody: int64(p.Cfg.Limits.HTTPMaxBody), Timeout: time.Duration(p.Cfg.Limits.HTTPTimeout),
	}
	if s.Read != nil {
		o.Resource, o.Tool, o.ToolArgs = s.Read.Resource, s.Read.Tool, s.Read.Args
	}
	if s.Auth != nil {
		o.Bearer = s.Auth.Bearer.Value
	}
	return o
}

// nudgeWorkers wakes an idle worker without blocking (no-op outside Serve).
func (p *Pipeline) nudgeWorkers() {
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}
