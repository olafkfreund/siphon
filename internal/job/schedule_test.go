package job

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// fakeClock is a manual clock: After registers a timer and announces its
// deadline on waits; advance moves time and releases the timers that are due.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	ts    []fakeTimer
	waits chan time.Time
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, waits: make(chan time.Time, 100)}
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if d <= 0 {
		ch <- c.now
	} else {
		c.ts = append(c.ts, fakeTimer{at, ch})
	}
	c.waits <- at
	return ch
}

func (c *fakeClock) advance(to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = to
	keep := c.ts[:0]
	for _, t := range c.ts {
		if !t.at.After(to) {
			t.ch <- to
		} else {
			keep = append(keep, t)
		}
	}
	c.ts = keep
}

// wait is the deadline of the loop's next timer (the loop is idle once it is set).
func (c *fakeClock) wait(t *testing.T) time.Time {
	t.Helper()
	select {
	case at := <-c.waits:
		return at
	case <-time.After(10 * time.Second):
		t.Fatal("scheduler never armed a timer")
		return time.Time{}
	}
}

func schedPipeline(t *testing.T, yaml string, clk *fakeClock) (*Pipeline, *config.Config) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := New(cfg, st, clk.Now)
	p.After = clk.After
	return p, cfg
}

func schedYAML(at, tz, extra string) string {
	return `
sources:
  tick: { type: schedule, at: "` + at + `", timezone: ` + tz + extra + ` }
rules:
  - { name: r, source: tick, when: "true", on: each, id: event.scheduled_at, action: { cmd: [echo, "{{.event.who}}"] } }
`
}

func startLoop(p *Pipeline, cfg *config.Config) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.scheduleLoop(ctx, cfg, "tick"); close(done) }()
	return func() { cancel(); <-done }
}

func rowCount(t *testing.T, p *Pipeline, q string, args ...any) (n int) {
	t.Helper()
	if err := p.Store.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// run drives the loop through every moment up to end and returns the moments
// that fired (seen ids, UTC).
func run(t *testing.T, tz, at string, start, end time.Time) []string {
	t.Helper()
	clk := newClock(start)
	p, cfg := schedPipeline(t, schedYAML(at, tz, ""), clk)
	stop := startLoop(p, cfg)
	defer stop()
	for {
		d := clk.wait(t)
		if d.After(end) {
			break
		}
		clk.advance(d)
	}
	rows, err := p.Store.DB.Query(`SELECT id FROM seen_event WHERE scope='schedule:tick' ORDER BY seen_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleFiresJobWithEventFields(t *testing.T) {
	clk := newClock(utc("2026-03-01T05:00:00Z"))
	p, cfg := schedPipeline(t, schedYAML("0 6 * * *", "Europe/London", ", data: { who: world }"), clk)
	stop := startLoop(p, cfg)
	defer stop()
	at := clk.wait(t)
	if at != utc("2026-03-01T06:00:00Z") {
		t.Fatalf("first moment %s", at)
	}
	if n := rowCount(t, p, `SELECT COUNT(*) FROM jobs`); n != 0 {
		t.Fatalf("new source fired for the past: %d jobs", n)
	}
	clk.advance(at)
	clk.wait(t) // re-armed: the fire is done
	var payload string
	if err := p.Store.DB.QueryRow(`SELECT action_json FROM jobs`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"who":"world"`, `"schedule":"tick"`, `"timezone":"Europe/London"`, `"scheduled_at":"2026-03-01T06:00:00Z"`, `"catch_up":false`, `"fired_at"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload lacks %s: %s", want, payload)
		}
	}
	if ev := rowCount(t, p, `SELECT COUNT(*) FROM source_state WHERE source='tick' AND last_poll_at=?`, at.UnixMilli()); ev != 1 {
		t.Error("last_poll_at is not the fired moment")
	}
}

func TestScheduleDST(t *testing.T) {
	// Spring forward 2026-03-29: 01:30 London does not exist, so that day does
	// not run (pinned robfig behaviour). The ids are local wall-clock times.
	got := run(t, "Europe/London", "30 1 * * *", utc("2026-03-27T12:00:00Z"), utc("2026-03-31T00:00:00Z"))
	want := "2026-03-28T01:30:00,2026-03-30T01:30:00"
	if strings.Join(got, ",") != want {
		t.Errorf("spring forward: got %v want %s", got, want)
	}
	// Fall back 2026-10-25: 01:30 happens twice; it fires once.
	got = run(t, "Europe/London", "30 1 * * *", utc("2026-10-24T12:00:00Z"), utc("2026-10-26T12:00:00Z"))
	want = "2026-10-25T01:30:00,2026-10-26T01:30:00"
	if strings.Join(got, ",") != want {
		t.Errorf("fall back: got %v want %s", got, want)
	}
}

func TestScheduleMonthEnd(t *testing.T) {
	got := run(t, "UTC", "0 0 31 * *", utc("2026-04-01T00:00:00Z"), utc("2026-08-15T00:00:00Z"))
	want := "2026-05-31T00:00:00,2026-07-31T00:00:00"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v want %s", got, want)
	}
}

func TestScheduleCatchUp(t *testing.T) {
	for _, mode := range []string{"latest", "none"} {
		t.Run(mode, func(t *testing.T) {
			clk := newClock(utc("2026-03-04T07:00:00Z"))
			p, cfg := schedPipeline(t, schedYAML("0 6 * * *", "UTC", ", catch_up: "+mode), clk)
			if err := store.PutSourceState(p.Store.DB, "tick", utc("2026-03-01T06:00:00Z"), ""); err != nil {
				t.Fatal(err)
			}
			stop := startLoop(p, cfg)
			defer stop()
			if next := clk.wait(t); next != utc("2026-03-05T06:00:00Z") {
				t.Fatalf("next %s", next)
			}
			var detail string
			p.Store.DB.QueryRow(`SELECT detail FROM audit WHERE event='schedule_skipped'`).Scan(&detail)
			if mode == "latest" {
				if rowCount(t, p, `SELECT COUNT(*) FROM jobs`) != 1 {
					t.Fatal("latest must fire once")
				}
				var payload string
				p.Store.DB.QueryRow(`SELECT action_json FROM jobs`).Scan(&payload)
				if !strings.Contains(payload, `"catch_up":true`) || !strings.Contains(payload, `"scheduled_at":"2026-03-04T06:00:00Z"`) {
					t.Errorf("payload %s", payload)
				}
				if !strings.Contains(detail, `"count":2`) || !strings.Contains(detail, `"to":"2026-03-03T06:00:00Z"`) {
					t.Errorf("audit %s", detail)
				}
			} else {
				if rowCount(t, p, `SELECT COUNT(*) FROM jobs`) != 0 {
					t.Fatal("none must not fire")
				}
				if !strings.Contains(detail, `"count":3`) {
					t.Errorf("audit %s", detail)
				}
			}
		})
	}
}

func TestScheduleRestartNoDuplicate(t *testing.T) {
	clk := newClock(utc("2026-03-01T05:00:00Z"))
	p, cfg := schedPipeline(t, schedYAML("0 6 * * *", "UTC", ""), clk)
	stop := startLoop(p, cfg)
	at := clk.wait(t)
	clk.advance(at)
	clk.wait(t)
	stop()
	// A crash between the job commit and the state write: the state is stale.
	if err := store.PutSourceState(p.Store.DB, "tick", utc("2026-03-01T04:00:00Z"), ""); err != nil {
		t.Fatal(err)
	}
	clk.advance(utc("2026-03-01T06:00:30Z"))
	stop = startLoop(p, cfg)
	defer stop()
	if next := clk.wait(t); next != utc("2026-03-02T06:00:00Z") {
		t.Fatalf("next %s", next)
	}
	if n := rowCount(t, p, `SELECT COUNT(*) FROM jobs`); n != 1 {
		t.Fatalf("%d jobs, want 1", n)
	}
}

func TestScheduleReloadKeepsNextMoment(t *testing.T) {
	clk := newClock(utc("2026-03-01T05:00:00Z"))
	p, cfg := schedPipeline(t, schedYAML("0 6 * * *", "UTC", ""), clk)
	stop := startLoop(p, cfg)
	first := clk.wait(t)
	stop()
	stop = startLoop(p, cfg) // what Apply does
	defer stop()
	if again := clk.wait(t); again != first {
		t.Fatalf("next moment moved: %s then %s", first, again)
	}
	if n := rowCount(t, p, `SELECT COUNT(*) FROM jobs`); n != 0 {
		t.Fatalf("reload fired %d jobs", n)
	}
}

func TestScheduleStateFromOldSourceIsNew(t *testing.T) {
	clk := newClock(utc("2026-03-04T07:00:00Z"))
	p, cfg := schedPipeline(t, schedYAML("0 6 * * *", "UTC", ""), clk)
	store.PutSourceState(p.Store.DB, "tick", utc("2026-03-01T06:00:00Z"), "")
	store.SetSourceEvent(p.Store.DB, "tick", map[string]any{"title": "old http event"})
	stop := startLoop(p, cfg)
	defer stop()
	clk.wait(t)
	if n := rowCount(t, p, `SELECT COUNT(*) FROM jobs`); n != 0 {
		t.Fatalf("fired %d for the past", n)
	}
}
