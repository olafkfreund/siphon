package job

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

// liveWindow is how late a moment may be and still count as live, not missed.
const liveWindow = 2 * time.Minute

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
		case <-p.after(min(next.Sub(p.Now()), time.Minute)): // re-sync each minute: a suspend or clock jump fires on time
		}
	}
}

// scheduleSync handles every moment in (last_poll_at, now]: the latest fires
// once, older ones are audited as skipped. A source with no state yet records
// now and fires nothing for the past. With catch_up: none, a startup fires
// nothing unless the moment is under liveWindow old. It returns the new last moment.
func (p *Pipeline) scheduleSync(ctx context.Context, cfg *config.Config, name string, sched cron.Schedule, loc *time.Location, startup bool) (time.Time, error) {
	now := p.Now()
	st, ok, err := store.SourceStateOf(p.Store.DB, name)
	if err != nil {
		return time.Time{}, err
	}
	// At startup, state that is not a schedule's own (another type's leftovers,
	// or no event at all) is a new source: record now, fire nothing for the past.
	if !ok || st.LastPollAt == nil || (startup && !p.hasScheduleEvent(name)) {
		return now, store.PutSourceState(p.Store.DB, name, now, "")
	}
	last := *st.LastPollAt
	first, prev, beforePrev, n := scheduleMoments(sched, loc, last, now)
	if n == 0 {
		return last, nil
	}
	late := now.Sub(prev) >= liveWindow
	fire := !(startup && late && cfg.Sources[name].CatchUp == "none")
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
	if err := p.scheduleFire(ctx, cfg, name, sched, loc, prev, startup && late); err != nil {
		store.PutSourceState(p.Store.DB, name, last, err.Error()) // health shows the error
		return last, err
	}
	return prev, nil
}

// scheduleMoments counts the moments in (last, now]: the first, the latest and
// the one before it. An interval is computed directly, so any outage is cheap.
func scheduleMoments(sched cron.Schedule, loc *time.Location, last, now time.Time) (first, prev, beforePrev time.Time, n int) {
	first = sched.Next(last.In(loc))
	if first.IsZero() || first.After(now) {
		return first, prev, beforePrev, 0
	}
	if cd, ok := sched.(cron.ConstantDelaySchedule); ok {
		k := int(now.Sub(first) / cd.Delay)
		prev = first.Add(time.Duration(k) * cd.Delay)
		return first, prev, prev.Add(-cd.Delay), k + 1
	}
	for m := first; !m.IsZero() && !m.After(now); m = sched.Next(m.In(loc)) {
		beforePrev, prev = prev, m
		n++
	}
	return first, prev, beforePrev, n
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
func (p *Pipeline) scheduleFire(ctx context.Context, cfg *config.Config, name string, sched cron.Schedule, loc *time.Location, at time.Time, catchUp bool) error {
	s := cfg.Sources[name]
	data := s.ScheduleEvent(loc, at, p.Now(), catchUp)
	if _, err := json.Marshal(data); err != nil {
		return fmt.Errorf("event data: %w", err)
	}
	ev := rule.Event{Source: name, Data: data}
	// The dedupe id: the UTC instant, so every real moment runs; for hourly or
	// rarer cron, zone + wall-clock time, so a DST fall-back's repeated hour fires once.
	seenID := at.UTC().Format(time.RFC3339)
	if !config.ScheduleKeyedByInstant(sched) {
		seenID = loc.String() + "|" + at.In(loc).Format("2006-01-02T15:04:05")
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

// hasScheduleEvent is true when the stored last event is a schedule's own: JSON
// with schedule, scheduled_at and fired_at. Anything else (another type's
// event, none at all) means the state is not ours.
func (p *Pipeline) hasScheduleEvent(name string) bool {
	j, _ := store.SourceEvent(p.Store.DB, name)
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(j), &m) != nil {
		return false
	}
	for _, k := range []string{"schedule", "scheduled_at", "fired_at"} {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
