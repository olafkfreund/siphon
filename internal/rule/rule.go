// Package rule decides which events fire which rules. The same Evaluate is
// used by the daemon, run-once and `rules test` (dryRun).
package rule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

const evalTimeout = 100 * time.Millisecond

// Event is what a source hands to the rule engine.
type Event struct {
	Source  string
	Headers map[string]string
	Data    any
	Depth   int
}

// Fire is one rule firing. Env is the templating/expr env (event, item, headers, source).
type Fire struct {
	Rule string
	Key  string
	Item any
	Env  map[string]any
}

var (
	progMu sync.Mutex
	progs  = map[string]*vm.Program{}
)

// compile caches programs by source; config.Validate already proved they compile.
func compile(src string) (*vm.Program, error) {
	progMu.Lock()
	defer progMu.Unlock()
	if p, ok := progs[src]; ok {
		return p, nil
	}
	p, err := expr.Compile(src, expr.Env(map[string]any{}), expr.AllowUndefinedVariables())
	if err != nil {
		return nil, err
	}
	progs[src] = p
	return p, nil
}

// run evaluates src with the 100 ms timeout.
// ponytail: on timeout the goroutine is abandoned (expr can't be interrupted);
// expr's default memory budget bounds runaway loops.
func run(ctx context.Context, src string, env map[string]any) (any, error) {
	p, err := compile(src)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	type res struct {
		v   any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := vm.Run(p, env)
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("eval %q: %w", src, ctx.Err())
	}
}

// Evaluate returns the fires for ev against rule r.
//
// All state writes go through tx; the caller inserts jobs in the same tx and
// commits. With dryRun the writes run inside a savepoint that is rolled back
// before returning, so the result is identical but nothing persists.
//
// An item whose expressions error is skipped (no state change) and the errors
// are joined into err alongside the fires that did succeed.
//
// Cooldown is per rule across keys. A fire suppressed by cooldown leaves state
// untouched, so an edge still true (or an unseen each id) fires once the
// cooldown has passed.
func Evaluate(ctx context.Context, tx *sql.Tx, r config.Rule, ev Event, now time.Time, dryRun bool) (fires []Fire, err error) {
	if ev.Source != r.Source || (ev.Source == config.AgentResultSource && !r.AllowAgentEvents) {
		return nil, nil
	}
	if dryRun {
		if _, err := tx.Exec(`SAVEPOINT rule_dry`); err != nil {
			return nil, err
		}
		defer func() {
			_, e1 := tx.Exec(`ROLLBACK TO rule_dry`)
			_, e2 := tx.Exec(`RELEASE rule_dry`)
			err = errors.Join(err, e1, e2)
		}()
	}

	headers := map[string]any{}
	for k, v := range ev.Headers {
		headers[k] = v
	}
	base := map[string]any{"event": ev.Data, "headers": headers, "source": ev.Source, "item": nil}

	items := []any{nil}
	if r.ForEach != "" {
		v, err := run(ctx, r.ForEach, base)
		if err != nil {
			return nil, fmt.Errorf("rule %s for_each: %w", r.Name, err)
		}
		if items, err = toSlice(v); err != nil {
			return nil, fmt.Errorf("rule %s for_each: %w", r.Name, err)
		}
	}

	var errs []error
	for _, item := range items {
		env := map[string]any{"event": ev.Data, "headers": headers, "source": ev.Source, "item": item}
		f, fired, err := evalItem(ctx, tx, r, env, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %s: %w", r.Name, err))
			continue
		}
		if fired {
			f.Item = item
			fires = append(fires, f)
		}
	}
	return fires, errors.Join(errs...)
}

func evalItem(ctx context.Context, tx *sql.Tx, r config.Rule, env map[string]any, now time.Time) (Fire, bool, error) {
	v, err := run(ctx, r.When, env)
	if err != nil {
		return Fire{}, false, err
	}
	cur, ok := v.(bool)
	if !ok {
		return Fire{}, false, fmt.Errorf("when returned %T, want bool", v)
	}
	key := "-"
	if r.ID != "" {
		idv, err := run(ctx, r.ID, env)
		if err != nil {
			return Fire{}, false, err
		}
		if idv == nil {
			return Fire{}, false, errors.New("id evaluated to nil")
		}
		key = fmt.Sprint(idv)
	}
	f := Fire{Rule: r.Name, Key: key, Env: env}

	if r.On == "each" {
		if !cur {
			return f, false, nil
		}
		ok, err := admit(tx, r, key, now, true)
		return f, ok, err
	}

	st, err := store.GetRuleState(tx, r.Name, key)
	if err != nil {
		return f, false, err
	}
	if !cur {
		if st.Found && !st.LastValue {
			return f, false, nil
		}
		return f, false, store.PutRuleState(tx, r.Name, key, false, time.Time{})
	}
	refire := r.Repeat > 0 && !st.LastFired.IsZero() && now.Sub(st.LastFired) >= time.Duration(r.Repeat)
	if st.LastValue && !refire {
		return f, false, nil
	}
	ok, err = admit(tx, r, key, now, false)
	return f, ok, err
}

// admit applies the per-rule cooldown, the each-dedupe, and records the fire.
func admit(tx *sql.Tx, r config.Rule, key string, now time.Time, each bool) (bool, error) {
	if r.Cooldown > 0 {
		last, err := store.RuleLastFired(tx, r.Name)
		if err != nil {
			return false, err
		}
		if !last.IsZero() && now.Sub(last) < time.Duration(r.Cooldown) {
			return false, nil
		}
	}
	if each {
		isNew, err := store.MarkSeen(tx, "rule:"+r.Name, key, now)
		if err != nil || !isNew {
			return false, err
		}
	}
	return true, store.PutRuleState(tx, r.Name, key, true, now)
}

func toSlice(v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, fmt.Errorf("for_each returned %T, want a list", v)
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, nil
}
