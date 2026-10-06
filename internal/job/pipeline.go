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
	"sort"
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
	// MCPTransport, if set, supplies the transport for an mcp source (tests).
	MCPTransport func(source string) mcp.Transport

	runFn func(context.Context, store.QueuedJob) (string, int, string) // tests only
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
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	now := p.Now()
	var fires []rule.Fire
	var ids []int64
	var links []string
	var errs []error
	for _, r := range p.Cfg.Rules {
		fs, err := rule.Evaluate(ctx, tx, r, ev, now, dryRun)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", r.Name, err))
		}
		fires = append(fires, fs...)
		if dryRun {
			continue
		}
		for _, f := range fs {
			id, link, err := p.enqueue(tx, r, f, ev.Depth, now)
			if err != nil {
				return nil, nil, err
			}
			ids = append(ids, id)
			if link != "" {
				links = append(links, link)
			}
		}
	}
	if !dryRun {
		if err := tx.Commit(); err != nil {
			return nil, nil, err
		}
		for _, l := range links { // after commit, so a rolled-back job never logs a link
			slog.Info("approval required", "link", l)
		}
	}
	return fires, ids, errors.Join(errs...)
}

// enqueue inserts the job; for pending_approval it also creates the approval
// in the same tx and returns its link path.
func (p *Pipeline) enqueue(tx *sql.Tx, r config.Rule, f rule.Fire, depth int, now time.Time) (id int64, link string, err error) {
	b, err := json.Marshal(Payload{Action: r.Action, Env: f.Env})
	if err != nil {
		return 0, "", err
	}
	state := "queued"
	if p.needsApproval(r) {
		state = "pending_approval"
	}
	id, err = store.InsertJob(tx, store.Job{Rule: r.Name, ActionJSON: string(b), State: state, RunAfter: now, Depth: depth}, now)
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
	if err := ctx.Err(); err != nil {
		// Interrupted, not failed: leave the job running so the startup
		// requeue picks it up instead of losing the event.
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
	default:
		return "failed", -1, "action type not implemented yet (unit/agent/routine arrive in later plan phases)"
	}
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
