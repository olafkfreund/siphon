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
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/cred"
	"github.com/olafkfreund/siphon/internal/egress"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

// cmdTimeout bounds a cmd action; ponytail: fixed until a rule needs its own.
const cmdTimeout = 10 * time.Minute

type Pipeline struct {
	cfg   atomic.Pointer[config.Config] // read via Config(); swapped by Apply
	Store *store.Store
	Now   func() time.Time
	nudge chan struct{} // set by Serve; wakes idle workers after enqueues
	// MCPTransport, if set, supplies the transport for an mcp source (tests).
	MCPTransport func(source string) mcp.Transport

	runFn func(context.Context, store.QueuedJob) (string, int, string) // tests only
	rnd   func() float64                                               // retry jitter source; tests inject

	egress *egress.Proxy // set by startEgress when any run has an allowlist

	applyMu     sync.Mutex // serialises Apply and Serve's poller start
	serveCtx    context.Context
	stopPollers func() // nil unless Serve is running
}

// Config is the current config. Each poll, event and job takes one snapshot
// for its whole run; Apply swaps it for the next one.
func (p *Pipeline) Config() *config.Config { return p.cfg.Load() }

// Apply makes cfg the live config: the pointer is swapped, then (under Serve)
// the pollers restart on the new sources. Queued jobs keep their payload
// snapshots; the worker count and egress listener are fixed at startup.
func (p *Pipeline) Apply(cfg *config.Config) error {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	p.cfg.Store(cfg)
	if p.stopPollers != nil {
		p.stopPollers()
		p.stopPollers = p.startPollers(p.serveCtx, cfg)
	}
	return nil
}

// runUnit starts a unit and waits for it; tests stub it. The step/action
// timeout arrives as the ctx deadline (cmdTimeout if there is none).
var runUnit = func(ctx context.Context, p *Pipeline, unit string) (int, string) {
	timeout := cmdTimeout
	if d, ok := ctx.Deadline(); ok {
		timeout = time.Until(d)
	}
	cfg := p.Config()
	exit, out, err := action.RunUnit(ctx, unit, cfg.Units, timeout, cfg.Secrets())
	if err != nil {
		return exit, string(out) + err.Error()
	}
	return exit, string(out)
}

func routineSteps(cfg *config.Config, a config.Action) []config.Step {
	if rt := cfg.Routines[a.Routine]; a.Routine != "" && rt != nil {
		return slices.Clone(rt.Steps)
	}
	return nil
}

// New returns a Pipeline ready for Serve. The worker nudge channel is made
// here, before any HTTP handler can call deliver (no race with Serve).
func New(cfg *config.Config, st *store.Store, now func() time.Time) *Pipeline {
	p := &Pipeline{Store: st, Now: now, nudge: make(chan struct{}, 64)}
	p.cfg.Store(cfg)
	return p
}

// Payload is what a job row carries: the action and the env it renders with.
type Payload struct {
	Action config.Action  `json:"action"`
	Env    map[string]any `json:"env"`
	// Agent marks jobs that run an agent (directly or in a routine step); the
	// daily agent cap counts them.
	Agent bool `json:"agent,omitempty"`
	// Steps is a snapshot of the routine's steps at enqueue time; running a
	// routine never reads the live config.
	Steps []config.Step `json:"steps,omitempty"`
	// Agents snapshots the agent definitions this job uses, so a paused or
	// queued job never picks up an edited agent. MCP sources stay live: they
	// hold resolved secrets that must not be persisted.
	Agents map[string]config.Agent `json:"agents,omitempty"`
}

// agentDef returns the snapshotted agent definition, falling back to the
// live config for payloads written before snapshots existed.
func agentDef(cfg *config.Config, name string, snap map[string]config.Agent) *config.Agent {
	if a, ok := snap[name]; ok {
		return &a
	}
	return cfg.Agents[name]
}

// agentSnapshot collects the definitions of every agent an action can run.
func agentSnapshot(cfg *config.Config, a config.Action) map[string]config.Agent {
	names := []string{a.Agent}
	for _, st := range routineSteps(cfg, a) {
		names = append(names, st.Agent)
	}
	out := map[string]config.Agent{}
	for _, n := range names {
		if def := cfg.Agents[n]; n != "" && def != nil {
			out[n] = *def
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Poll fetches one event from a polled source.
func (p *Pipeline) Poll(ctx context.Context, name string) (rule.Event, error) {
	return p.poll(ctx, p.Config(), name)
}

func (p *Pipeline) poll(ctx context.Context, cfg *config.Config, name string) (rule.Event, error) {
	s := cfg.Sources[name]
	if s == nil {
		return rule.Event{}, fmt.Errorf("unknown source %q", name)
	}
	var ev source.Event
	var err error
	switch s.Type {
	case "http":
		ev, err = source.HTTP{Options: source.HTTPOptions{
			Name: name, URL: s.URL, Method: s.Method, Headers: headerValues(s.Headers), Body: []byte(s.Body),
			AllowPrivate: s.AllowPrivate, MaxBody: int64(cfg.Limits.HTTPMaxBody), Timeout: time.Duration(cfg.Limits.HTTPTimeout),
		}}.Poll(ctx)
	case "mcp":
		o := mcpOptions(cfg, name)
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
	return p.tick(ctx, p.Config(), name)
}

func (p *Pipeline) tick(ctx context.Context, cfg *config.Config, name string) ([]int64, error) {
	ev, err := p.poll(ctx, cfg, name)
	msg := ""
	if err != nil {
		msg = string(action.Mask([]byte(err.Error()), cfg.Secrets()))
	}
	if serr := store.PutSourceState(p.Store.DB, name, p.Now(), msg); serr != nil {
		return nil, serr
	}
	if err != nil {
		return nil, err
	}
	if serr := store.SetSourceEvent(p.Store.DB, name, ev.Data); serr != nil {
		slog.Warn("store last event", "source", name, "err", serr)
	}
	_, ids, _, ruleErr, err := p.handleEvent(ctx, cfg, ev, false, "", "")
	return ids, errors.Join(err, ruleErr)
}

// HandleEvent evaluates every rule against ev in one transaction and, unless
// dryRun, enqueues a job per fire. Rule state and job inserts commit together.
func (p *Pipeline) HandleEvent(ctx context.Context, ev rule.Event, dryRun bool) ([]rule.Fire, []int64, error) {
	fires, ids, _, ruleErr, err := p.handleEvent(ctx, p.Config(), ev, dryRun, "", "")
	return fires, ids, errors.Join(err, ruleErr)
}

// handleEvent is HandleEvent plus an optional replay check: when seenScope is
// set, (seenScope, seenID) is recorded in the same transaction as the jobs, and
// dup=true is returned without evaluating anything if it was already seen.
// ruleErr holds per-rule evaluation errors from a transaction that still
// committed; err means nothing was committed.
func (p *Pipeline) handleEvent(ctx context.Context, cfg *config.Config, ev rule.Event, dryRun bool, seenScope, seenID string) (fires []rule.Fire, ids []int64, dup bool, ruleErr, err error) {
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
	for _, r := range cfg.Rules {
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
			id, link, err := p.enqueue(tx, cfg, r, f, ev, now)
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
			slog.Info("approval required", "job", id, "approve", fmt.Sprintf("siphon approve %d", id))
		}
	}
	return fires, ids, false, errors.Join(errs...), nil
}

// enqueue inserts the job; for pending_approval it also creates the approval
// in the same tx and returns its link path.
func (p *Pipeline) enqueue(tx *sql.Tx, cfg *config.Config, r config.Rule, f rule.Fire, ev rule.Event, now time.Time) (id int64, link string, err error) {
	depth := ev.Depth
	if depth > config.MaxDepth {
		return 0, "", store.Audit(tx, now, "rule:"+r.Name, "skip_depth", 0, fmt.Sprintf("depth %d > %d", depth, config.MaxDepth))
	}
	hasAgent := r.Action.Agent != "" || cfg.RoutineHasAgent(r.Action.Routine)
	if hasAgent {
		n, err := store.CountAgentJobsSince(tx, now.Add(-24*time.Hour))
		if err != nil {
			return 0, "", err
		}
		if n >= cfg.Limits.AgentRunsPerDay {
			return 0, "", store.Audit(tx, now, "rule:"+r.Name, "skip_agent_cap", 0, fmt.Sprintf("%d agent runs in 24h", n))
		}
	}
	b, err := json.Marshal(Payload{Action: r.Action, Env: f.Env, Agent: hasAgent, Steps: routineSteps(cfg, r.Action), Agents: agentSnapshot(cfg, r.Action)})
	if err != nil {
		return 0, "", err
	}
	state := "queued"
	if needsApproval(cfg, r) {
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

func needsApproval(cfg *config.Config, r config.Rule) bool { return cfg.NeedsApproval(r) }

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
	if state == statePaused || state == stateRequeued {
		return true, nil // routine parked (approval) or rescheduled (retry); the store already recorded it
	}
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
	cfg := p.Config()
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
		opts := sandbox(cfg, cmdTimeout)
		allow, on := ruleEgress(cfg, j.Rule)
		var finish func() string
		if opts.Egress, finish, err = p.egressFor(cfg, j.ID, allow, on); err != nil {
			return "failed", -1, err.Error()
		}
		code, out, err := action.RunCmd(ctx, argv, opts, cfg.Secrets())
		tail := finish()
		if err != nil {
			return "failed", code, string(out) + err.Error() + tail
		}
		if code != 0 {
			return "failed", code, string(out) + tail
		}
		return "done", 0, string(out) + tail
	case pl.Action.Agent != "":
		return p.runAgent(ctx, cfg, j, pl)
	case pl.Action.Unit != "":
		uctx, cancel := context.WithTimeout(ctx, cmdTimeout)
		defer cancel()
		if code, out := runUnit(uctx, p, pl.Action.Unit); code != 0 {
			return "failed", code, out
		} else {
			return "done", 0, out
		}
	case pl.Action.Routine != "":
		return p.runRoutine(ctx, cfg, j, pl)
	default:
		return "failed", -1, "job has no action"
	}
}

// runAgent runs the agent and feeds its JSON result back as an agent-result
// event one level deeper; rules see it only with allow_agent_events (loop guard).
func (p *Pipeline) runAgent(ctx context.Context, cfg *config.Config, j store.QueuedJob, pl Payload) (string, int, string) {
	state, exit, out, _ := p.agentExec(ctx, cfg, j, pl, false)
	return state, exit, out
}

// agentExec is runAgent that also returns the agent's stdout (routine steps parse it).
// wait=false (plain agent jobs) puts the job back in the queue when its login
// is busy instead of tying up a worker; routine steps (wait=true) block.
func (p *Pipeline) agentExec(ctx context.Context, cfg *config.Config, j store.QueuedJob, pl Payload, wait bool) (string, int, string, []byte) {
	a := agentDef(cfg, pl.Action.Agent, pl.Agents)
	if a == nil {
		return "failed", -1, "unknown agent " + pl.Action.Agent, nil
	}
	servers := map[string]action.MCPServer{}
	for _, name := range a.MCP {
		s := cfg.Sources[name]
		h := headerValues(s.Headers)
		if s.Auth != nil && s.Auth.Bearer.Value != "" {
			h["Authorization"] = "Bearer " + s.Auth.Bearer.Value
		}
		servers[name] = action.MCPServer{URL: s.URL, Command: s.Command, Headers: h}
	}
	opts := action.AgentOptions{
		Kind: a.Kind, Command: a.Command, Runner: a.Runner,
		Prompt: a.Prompt, Env: pl.Env, MCP: servers,
		AllowedTools: a.AllowedTools, MaxTurns: a.MaxTurns, MaxBudgetUSD: a.MaxBudgetUSD,
		Timeout: time.Duration(a.Timeout), Sandbox: sandbox(cfg, 0),
		Secrets: cfg.Secrets(), WorkDir: filepath.Join(filepath.Dir(cfg.Server.DB), "jobs", strconv.FormatInt(j.ID, 10)),
	}
	// Credential: an API key, or a subscription login from the store. No
	// credential at all is the legacy path (the runner's own environment).
	credName, c := a.Credential, cfg.Credentials[a.Credential]
	st := cred.StoreFor(cfg)
	var start map[string][]byte
	switch {
	case c == nil:
	case c.APIKey.Value != "":
		opts.APIKey = c.APIKey.Value
	default:
		// One run per login at a time (default): rotating refresh tokens
		// would otherwise invalidate each other.
		actx := ctx
		if !wait {
			var cancel context.CancelFunc
			actx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
		}
		release, err := cred.Acquire(actx, credName, c.Concurrency)
		if err != nil && !wait && ctx.Err() == nil {
			if rerr := store.RequeueJob(p.Store.DB, j.ID, j.ResumeStep, j.Output, p.Now().Add(credBusyRetry)); rerr != nil {
				return "failed", -1, rerr.Error(), nil
			}
			return stateRequeued, 0, "", nil
		}
		if err != nil {
			return "failed", -1, "waiting for credential " + credName + ": " + err.Error(), nil
		}
		defer release()
		files, err := st.Load(credName)
		if err != nil || len(files) == 0 {
			return "failed", 1, fmt.Sprintf("credential %s is not imported: run `siphon credentials import %s < <login file>`", credName, credName), nil
		}
		opts.CredFiles, start = files, files
	}
	allow, on := cfg.AgentEgress(a)
	egEnv, finish, eerr := p.egressFor(cfg, j.ID, allow, on)
	if eerr != nil {
		return "failed", -1, eerr.Error(), nil
	}
	opts.Sandbox.Egress = egEnv
	res, err := action.RunAgent(ctx, opts)
	egressTail := finish()
	if c != nil {
		p.saveWriteback(cfg, j, credName, c.Provider, start, res.Writeback)
	}
	out := string(res.Output) + egressTail
	if err != nil {
		return "failed", res.Exit, out + err.Error(), nil
	}
	if res.Exit != 0 {
		// Classify only failures: a successful answer may well mention "401".
		switch res.Class {
		case "auth":
			msg := fmt.Sprintf("credential %s needs re-login: log in with %s on the host, then run `siphon credentials import %s`", credName, a.Kind, credName)
			p.audit("credential_reauth", j.ID, credName)
			return "failed", res.Exit, msg + "\n" + out, res.Stdout
		case "quota":
			return "failed", res.Exit, "quota/rate limit reached for " + credName + "\n" + out, res.Stdout
		}
		return "failed", res.Exit, out, res.Stdout
	}
	// One agent-result shape for every kind: {kind, result, raw}. A JSON-object
	// answer keeps its own fields at the top too, so existing rules still match.
	data := map[string]any{}
	if m, ok := res.Raw.(map[string]any); ok {
		for k, v := range m {
			data[k] = v
		}
	}
	data["kind"], data["result"], data["raw"] = a.Kind, res.Result, res.Raw
	ev := rule.Event{Source: config.AgentResultSource, Depth: j.Depth + 1, ParentID: j.ID, Data: data}
	_, _, _, ruleErr, err := p.handleEvent(ctx, cfg, ev, false, "", "")
	if err != nil {
		// Nothing committed: failing beats a "done" job whose result vanished.
		return "failed", 1, "agent result not recorded: " + err.Error() + "\n" + out, res.Stdout
	}
	if ruleErr != nil {
		slog.Warn("agent-result rules", "job", j.ID, "err", ruleErr)
	}
	return "done", 0, out, res.Stdout
}

// credBusyRetry is how long a plain agent job waits in the queue when its
// login is busy. ponytail: fixed; tune if logins queue deeply.
const credBusyRetry = 5 * time.Second

// saveWriteback stores refreshed login files: shape-checked, then
// compare-and-swap against what this run started with.
func (p *Pipeline) saveWriteback(cfg *config.Config, j store.QueuedJob, credName, provider string, start, wb map[string][]byte) {
	st := cred.StoreFor(cfg)
	for file, b := range wb {
		_, norm, err := cred.ValidateFor(provider, true, b)
		if err != nil {
			slog.Warn("credential write-back rejected", "credential", credName, "file", file, "err", err)
			continue
		}
		// A run must not swap the owner's login for another account (a
		// prompt-injected agent controls what it writes back).
		if !cred.SameAccount(provider, start[file], norm) {
			slog.Warn("credential write-back rejected: account changed", "credential", credName, "file", file)
			p.audit("credential_writeback_rejected", j.ID, credName)
			continue
		}
		wrote, err := st.Save(credName, file, start[file], norm)
		if err != nil {
			slog.Warn("credential write-back failed", "credential", credName, "err", err)
			continue
		}
		if wrote {
			p.audit("credential_refreshed", j.ID, credName)
		} else {
			// The store changed since this run started (a re-import): it wins.
			slog.Info("credential write-back stale; kept the stored login", "credential", credName)
			p.audit("credential_writeback_stale", j.ID, credName)
		}
	}
}

// audit writes one audit row outside any transaction.
func (p *Pipeline) audit(event string, jobID int64, detail string) {
	tx, err := p.Store.DB.Begin()
	if err != nil {
		return
	}
	if store.Audit(tx, p.Now(), "agent", event, jobID, detail) == nil {
		tx.Commit()
	} else {
		tx.Rollback()
	}
}

// RunOnce polls every polled source once, then runs whatever got queued.
func (p *Pipeline) RunOnce(ctx context.Context) error {
	if err := p.Requeue(); err != nil {
		return err
	}
	ectx, stopEgress := context.WithCancel(ctx)
	defer stopEgress()
	if err := p.startEgress(ectx, p.Config()); err != nil {
		return err
	}
	if err := p.ExpireApprovals(); err != nil {
		return err
	}
	var errs []error
	cfg := p.Config()
	for _, name := range sortedSources(cfg) {
		if cfg.Sources[name].Type == "webhook" {
			continue
		}
		if _, err := p.tick(ctx, cfg, name); err != nil {
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
		Action config.Action           `json:"action"`
		Env    json.RawMessage         `json:"env"`
		Agent  bool                    `json:"agent"`
		Steps  []config.Step           `json:"steps"`
		Agents map[string]config.Agent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return Payload{}, err
	}
	env, err := source.DecodeJSON(raw.Env)
	if err != nil {
		return Payload{}, err
	}
	m, _ := env.(map[string]any)
	return Payload{Action: raw.Action, Env: m, Agent: raw.Agent, Steps: raw.Steps, Agents: raw.Agents}, nil
}

func mcpOptions(cfg *config.Config, name string) source.MCPOptions {
	s := cfg.Sources[name]
	o := source.MCPOptions{
		Name: name, Command: s.Command, URL: s.URL, AllowPrivate: s.AllowPrivate,
		MaxBody: int64(cfg.Limits.HTTPMaxBody), Timeout: time.Duration(cfg.Limits.HTTPTimeout),
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

// sandbox is the action sandbox for this pipeline; template-unit runs keep
// their per-run directories in server.actions_dir (default: next to the DB).
func sandbox(cfg *config.Config, timeout time.Duration) action.SandboxOptions {
	dir := cfg.Server.ActionsDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(cfg.Server.DB), "actions")
	}
	return action.SandboxOptions{Mode: cfg.Server.Sandbox, Timeout: timeout, Dir: dir}
}
