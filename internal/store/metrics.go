package store

import (
	"database/sql"
	"time"
)

// Metrics is a point-in-time read of queue, source and notification health for /metrics.
// It holds counts, ages and names only: no job ids, error texts or event data.
type Metrics struct {
	Jobs           map[string]int    // state -> count; every state present
	Finished       map[string]int    // done|failed|cancelled -> finished in the last hour
	QueueOldest    int64             // seconds since the oldest queued job was created, 0 if none
	Approvals      int               // jobs in pending_approval
	ApprovalOldest int64             // seconds, 0 if none
	AgentRuns      int               // agent jobs in the daily-cap window (the last 24h)
	SourceFailing  map[string]bool   // source -> polls failing
	SourcePoll     map[string]int64  // source -> last poll, unix seconds (absent if never)
	RuleError      map[string]bool   // rules with a rule_error row
	Notifications  map[[2]string]int // (channel, state) -> count
}

// GetMetrics runs every query in one read transaction.
func GetMetrics(db *sql.DB, now time.Time) (Metrics, error) {
	m := Metrics{
		Jobs:          map[string]int{"queued": 0, "running": 0, "pending_approval": 0, "done": 0, "failed": 0, "cancelled": 0},
		Finished:      map[string]int{"done": 0, "failed": 0, "cancelled": 0},
		SourceFailing: map[string]bool{}, SourcePoll: map[string]int64{}, RuleError: map[string]bool{},
		Notifications: map[[2]string]int{},
	}
	tx, err := db.Begin()
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	// each runs one query and calls scan per row
	each := func(q string, args []any, scan func(*sql.Rows) error) error {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	err = each(`SELECT state, count(*) FROM jobs GROUP BY state`, nil, func(r *sql.Rows) (err error) {
		var s string
		var n int
		if err = r.Scan(&s, &n); err == nil {
			m.Jobs[s] = n
		}
		return
	})
	if err == nil {
		err = each(`SELECT state, count(*) FROM jobs WHERE state IN ('done','failed','cancelled') AND finished_at >= ? GROUP BY state`,
			[]any{ms(now.Add(-time.Hour))}, func(r *sql.Rows) (err error) {
				var s string
				var n int
				if err = r.Scan(&s, &n); err == nil {
					m.Finished[s] = n
				}
				return
			})
	}
	age := func(state string) (int64, error) {
		var c sql.NullInt64
		err := tx.QueryRow(`SELECT min(created_at) FROM jobs WHERE state=?`, state).Scan(&c)
		if err != nil || !c.Valid {
			return 0, err
		}
		return max(0, (ms(now)-c.Int64)/1000), nil
	}
	if err == nil {
		m.Approvals = m.Jobs["pending_approval"]
		if m.QueueOldest, err = age("queued"); err == nil {
			m.ApprovalOldest, err = age("pending_approval")
		}
	}
	if err == nil {
		m.AgentRuns, err = CountAgentJobsSince(tx, now.Add(-24*time.Hour)) // same window as the cap (job/pipeline.go)
	}
	if err == nil {
		err = each(`SELECT source, failing_since IS NOT NULL, last_poll_at FROM source_state`, nil, func(r *sql.Rows) error {
			var s string
			var f bool
			var p sql.NullInt64
			if err := r.Scan(&s, &f, &p); err != nil {
				return err
			}
			m.SourceFailing[s] = f
			if p.Valid {
				m.SourcePoll[s] = p.Int64 / 1000
			}
			return nil
		})
	}
	if err == nil {
		err = each(`SELECT rule FROM rule_error`, nil, func(r *sql.Rows) error {
			var s string
			if err := r.Scan(&s); err != nil {
				return err
			}
			m.RuleError[s] = true
			return nil
		})
	}
	if err == nil {
		err = each(`SELECT channel, state, count(*) FROM notifications GROUP BY channel, state`, nil, func(r *sql.Rows) error {
			var c, s string
			var n int
			if err := r.Scan(&c, &s, &n); err != nil {
				return err
			}
			m.Notifications[[2]string{c, s}] = n
			return nil
		})
	}
	return m, err
}
