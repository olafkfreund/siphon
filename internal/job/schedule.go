package job

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

// maxScheduleScan caps the moments counted when catching up after downtime.
// ponytail: past the cap the "latest" moment is stale; 100k covers every 1m for ~70 days.
const maxScheduleScan = 100000

// after is the timer seam: tests set Pipeline.After, production uses time.After.
func (p *Pipeline) after(d time.Duration) <-chan time.Time {
	if p.After != nil {
		return p.After(d)
	}
	return time.After(d)
}

// scheduleLoop fires a schedule source at each of its moments until ctx ends.
// The state (source_state.last_poll_at = the last moment handled) is the only
// memory, so a restart or a live reload resumes exactly where it stopped.
func (p *Pipeline) scheduleLoop(ctx context.Context, cfg *config.Config, name string) {
	s := cfg.Sources[name]
	sched, err := config.ParseSchedule(s.At)
	loc, lerr := s.Location()
	if err != nil || lerr != nil {
		slog.Warn("schedule source is invalid", "source", name, "err", err, "tz_err", lerr)
		return
	}
	for startup := true; ; startup = false {
		last, err := p.scheduleSync(ctx, cfg, name, sched, loc, startup)
		if err != nil && ctx.Err() == nil {
			slog.Warn("schedule failed", "source", name, "err", err)
		}
		next := sched.Next(last.In(loc))
		if err != nil || last.IsZero() || next.IsZero() {
			next = p.Now().Add(time.Minute) // retry later; never spin
		}
		select {
		case <-ctx.Done():
			return
		case <-p.after(next.Sub(p.Now())):
		}
	}
}

// scheduleSync handles every moment in (last_poll_at, now]: the latest fires
// once (catch_up: true at startup), older ones are audited as skipped. A source
// with no state yet records now and fires nothing for the past. With
// catch_up: none, a startup fires nothing. It returns the new last moment.
func (p *Pipeline) scheduleSync(ctx context.Context, cfg *config.Config, name string, sched cron.Schedule, loc *time.Location, startup bool) (time.Time, error) {
	now := p.Now()
	states, err := store.SourceStates(p.Store.DB)
	if err != nil {
		return time.Time{}, err
	}
	st, ok := states[name]
	if !ok || st.LastPollAt == nil || !p.hasScheduleEvent(name) {
		return now, store.PutSourceState(p.Store.DB, name, now, "")
	}
	last := *st.LastPollAt
	var first, prev, beforePrev time.Time
	n := 0
	for t := last; n < maxScheduleScan; {
		m := sched.Next(t.In(loc))
		if m.IsZero() || m.After(now) {
			break
		}
		if n == 0 {
			first = m
		}
		beforePrev, prev, t = prev, m, m
		n++
	}
	if n == 0 {
		return last, nil
	}
	s := cfg.Sources[name]
	fire := !(startup && s.CatchUp == "none")
	skipped, to := n, prev
	if fire {
		skipped, to = n-1, beforePrev
	}
	if skipped > 0 {
		detail, _ := json.Marshal(map[string]any{"source": name, "count": skipped,
			"from": first.In(loc).Format(time.RFC3339), "to": to.In(loc).Format(time.RFC3339)})
		if err := p.auditSystem("schedule_skipped", string(detail)); err != nil {
			return last, err
		}
	}
	if !fire {
		return prev, store.PutSourceState(p.Store.DB, name, prev, "")
	}
	return prev, p.scheduleFire(ctx, cfg, name, loc, prev, startup)
}

func (p *Pipeline) auditSystem(event, detail string) error {
	tx, err := p.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.Audit(tx, p.Now(), "system", event, 0, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// scheduleFire runs the rules for one moment. (schedule:<name>, moment) is
// recorded with the jobs, so a moment fires at most once, even across restarts.
func (p *Pipeline) scheduleFire(ctx context.Context, cfg *config.Config, name string, loc *time.Location, at time.Time, catchUp bool) error {
	s := cfg.Sources[name]
	data := s.ScheduleEvent(name, loc, at, p.Now(), catchUp)
	ev := rule.Event{Source: name, Data: data}
	// The dedupe id is the UTC instant, except for cron: its wall-clock time, so
	// the repeated hour of a DST fall-back fires once, not twice.
	seenID := at.UTC().Format(time.RFC3339)
	if !strings.HasPrefix(s.At, "every ") {
		seenID = at.In(loc).Format("2006-01-02T15:04:05")
	}
	_, ids, _, ruleErr, err := p.handleEvent(ctx, cfg, ev, false, "schedule:"+name, seenID)
	if err != nil {
		return err
	}
	if serr := store.PutSourceState(p.Store.DB, name, at, ""); serr != nil {
		return serr
	}
	if serr := store.SetSourceEvent(p.Store.DB, name, data); serr != nil {
		slog.Warn("store last event", "source", name, "err", serr)
	}
	for range ids {
		p.nudgeWorkers()
	}
	if ruleErr != nil {
		slog.Warn("schedule rule failed", "source", name, "err", ruleErr)
	}
	return nil
}

// hasScheduleEvent is false for state left by an earlier non-schedule source
// of the same name (it has a last event without scheduled_at): a new source.
// No event at all is fine: a schedule that has not fired yet.
func (p *Pipeline) hasScheduleEvent(name string) bool {
	j, _ := store.SourceEvent(p.Store.DB, name)
	return j == "" || strings.Contains(j, `"scheduled_at"`)
}
