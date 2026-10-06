package rule

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

var t0 = time.Unix(1_700_000_000, 0)

type harness struct {
	t *testing.T
	s *store.Store
}

func newH(t *testing.T) *harness {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &harness{t, s}
}

func (h *harness) eval(r config.Rule, ev Event, at time.Duration, dry bool) []Fire {
	h.t.Helper()
	tx, _ := h.s.DB.Begin()
	f, err := Evaluate(context.Background(), tx, r, ev, t0.Add(at), dry)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return f
}

func ev(data any) Event { return Event{Source: "s", Data: data} }
func val(n int) Event   { return ev(map[string]any{"v": n}) }

func TestEdge(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", When: "event.v > 5"}
	// v sequence and expected fires: false, true(fire), true, true, false, true(fire)
	for i, c := range []struct {
		v    int
		want int
	}{{1, 0}, {9, 1}, {9, 0}, {9, 0}, {1, 0}, {9, 1}} {
		if got := len(h.eval(r, val(c.v), time.Duration(i)*time.Minute, false)); got != c.want {
			t.Fatalf("step %d v=%d: fires=%d want %d", i, c.v, got, c.want)
		}
	}
}

func TestEdgeRepeat(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", When: "event.v > 5", Repeat: config.Duration(time.Hour)}
	for i, c := range []struct {
		at   time.Duration
		want int
	}{{0, 1}, {30 * time.Minute, 0}, {61 * time.Minute, 1}, {90 * time.Minute, 0}, {122 * time.Minute, 1}} {
		if got := len(h.eval(r, val(9), c.at, false)); got != c.want {
			t.Fatalf("step %d: fires=%d want %d", i, got, c.want)
		}
	}
}

func TestEachOncePerID(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", ForEach: "event.tasks", ID: "item.id", When: `item.status == "failed"`, On: "each"}
	mk := func(ids ...any) Event {
		var items []any
		for _, id := range ids {
			items = append(items, map[string]any{"id": id, "status": "failed"})
		}
		return ev(map[string]any{"tasks": items})
	}
	f := h.eval(r, mk(1, 2), 0, false)
	if len(f) != 2 || f[0].Key != "1" || f[1].Key != "2" || f[0].Item.(map[string]any)["id"] != 1 {
		t.Fatalf("%+v", f)
	}
	if f := h.eval(r, mk(1, 2, 3), time.Minute, false); len(f) != 1 || f[0].Key != "3" {
		t.Fatalf("%+v", f)
	}
}

func TestCooldownAcrossKeys(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", ForEach: "event.ids", ID: "item", When: "true", On: "each", Cooldown: config.Duration(10 * time.Minute)}
	e := func(ids ...any) Event { return ev(map[string]any{"ids": ids}) }
	if f := h.eval(r, e("a", "b"), 0, false); len(f) != 1 || f[0].Key != "a" {
		t.Fatalf("same-event second key must be held: %+v", f)
	}
	if f := h.eval(r, e("a", "b"), 5*time.Minute, false); len(f) != 0 {
		t.Fatalf("within cooldown: %+v", f)
	}
	if f := h.eval(r, e("a", "b"), 11*time.Minute, false); len(f) != 1 || f[0].Key != "b" {
		t.Fatalf("b fires after cooldown (a already seen): %+v", f)
	}
}

func TestEdgeCooldownKeepsEdgePending(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", When: "event.v > 5", Cooldown: config.Duration(10 * time.Minute)}
	h.eval(r, val(9), 0, false)
	h.eval(r, val(1), time.Minute, false) // false again
	if f := h.eval(r, val(9), 2*time.Minute, false); len(f) != 0 {
		t.Fatalf("cooldown should hold: %+v", f)
	}
	if f := h.eval(r, val(9), 11*time.Minute, false); len(f) != 1 {
		t.Fatalf("should fire once cooldown passes: %+v", f)
	}
}

func TestAgentResultGating(t *testing.T) {
	h := newH(t)
	e := Event{Source: config.AgentResultSource, Data: map[string]any{}, Depth: 1}
	r := config.Rule{Name: "r", Source: config.AgentResultSource, When: "true"}
	if f := h.eval(r, e, 0, false); len(f) != 0 {
		t.Fatalf("ignored by default: %+v", f)
	}
	r.AllowAgentEvents = true
	if f := h.eval(r, e, 0, false); len(f) != 1 {
		t.Fatalf("opt-in: %+v", f)
	}
}

func TestOtherSourceIgnored(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "other", When: "true"}
	if f := h.eval(r, val(1), 0, false); len(f) != 0 {
		t.Fatal(f)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", When: "true", On: "each", ID: `"x"`}
	if len(h.eval(r, val(1), 0, true)) != 1 {
		t.Fatal("dry run should report the fire")
	}
	if len(h.eval(r, val(1), 0, true)) != 1 {
		t.Fatal("dry run must not consume the each id")
	}
	if len(h.eval(r, val(1), 0, false)) != 1 || len(h.eval(r, val(1), 0, false)) != 0 {
		t.Fatal("real run fires once")
	}
}

func TestEnvAndHeaders(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", When: `headers["X-E"] == "pr" && source == "s" && event.n == 3`}
	f := h.eval(r, Event{Source: "s", Headers: map[string]string{"X-E": "pr"}, Data: map[string]any{"n": 3}}, 0, false)
	if len(f) != 1 || f[0].Env["source"] != "s" || f[0].Key != "-" {
		t.Fatalf("%+v", f)
	}
}

func TestBadItemErrorsOthersContinue(t *testing.T) {
	h := newH(t)
	r := config.Rule{Name: "r", Source: "s", ForEach: "event.xs", When: "item.n > 1", ID: "string(item.n)", On: "each"}
	tx, _ := h.s.DB.Begin()
	defer tx.Rollback()
	f, err := Evaluate(context.Background(), tx, r, ev(map[string]any{"xs": []any{map[string]any{"n": "bad"}, map[string]any{"n": 5}}}), t0, false)
	if err == nil || !strings.Contains(err.Error(), "rule r") || len(f) != 1 {
		t.Fatalf("fires=%+v err=%v", f, err)
	}
}

func TestNonBoolWhen(t *testing.T) {
	h := newH(t)
	tx, _ := h.s.DB.Begin()
	defer tx.Rollback()
	_, err := Evaluate(context.Background(), tx, config.Rule{Name: "r", Source: "s", When: "1 + 1"}, val(1), t0, false)
	if err == nil {
		t.Fatal("want error")
	}
}
