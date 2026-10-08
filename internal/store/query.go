package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Read-side helpers for the web API and portal.

type SourceState struct {
	Source     string     `json:"source"`
	LastPollAt *time.Time `json:"last_poll_at"`
	LastError  string     `json:"last_error"`
}

func SourceStates(db *sql.DB) (map[string]SourceState, error) {
	rows, err := db.Query(`SELECT source, last_poll_at, last_error FROM source_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SourceState{}
	for rows.Next() {
		var s SourceState
		var at sql.NullInt64
		if err := rows.Scan(&s.Source, &at, &s.LastError); err != nil {
			return nil, err
		}
		if at.Valid {
			t := time.UnixMilli(at.Int64)
			s.LastPollAt = &t
		}
		out[s.Source] = s
	}
	return out, rows.Err()
}

// RuleEnabled reports whether a rule may fire. Rules are enabled by default;
// overridden is true when an operator disabled it at runtime.
func RuleEnabled(tx *sql.Tx, name string) (enabled, overridden bool, err error) {
	var e int
	err = tx.QueryRow(`SELECT enabled FROM rule_override WHERE rule=?`, name).Scan(&e)
	if err == sql.ErrNoRows {
		return true, false, nil
	}
	return e != 0, err == nil, err
}

// SetRuleOverride disables a rule (row present) or re-enables it (row removed,
// back to the config default), and audits the change.
//
// Disabling also cancels the rule's queued and pending_approval jobs in the
// same transaction (audited as rule_disabled_cancel with the count).
// Re-enabling resumes from the frozen rule state: events seen while the rule
// was disabled were never recorded, so their ids may fire on re-enable.
func SetRuleOverride(db *sql.DB, name string, enabled bool, by string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	event := "rule_enable"
	if enabled {
		_, err = tx.Exec(`DELETE FROM rule_override WHERE rule=?`, name)
	} else {
		event = "rule_disable"
		_, err = tx.Exec(`INSERT INTO rule_override(rule, enabled, updated_at) VALUES (?,0,?)
			ON CONFLICT(rule) DO UPDATE SET enabled=0, updated_at=excluded.updated_at`, name, ms(now))
		if err == nil {
			var r sql.Result
			r, err = tx.Exec(`UPDATE jobs SET state='cancelled', finished_at=?
				WHERE rule=? AND state IN ('queued','pending_approval')`, ms(now), name)
			if err == nil {
				n, _ := r.RowsAffected()
				err = Audit(tx, now, by, "rule_disabled_cancel", 0, fmt.Sprintf("%s: %d jobs", name, n))
			}
		}
	}
	if err != nil {
		return err
	}
	if err := Audit(tx, now, by, event, 0, name); err != nil {
		return err
	}
	return tx.Commit()
}

// RuleOverrides lists the names of rules disabled at runtime.
func RuleOverrides(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT rule FROM rule_override WHERE enabled=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// RuleLastFiredAll is the latest fire time per rule.
func RuleLastFiredAll(db *sql.DB) (map[string]time.Time, error) {
	rows, err := db.Query(`SELECT rule, MAX(last_fired_at) FROM rule_state WHERE last_fired_at IS NOT NULL GROUP BY rule`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var n string
		var t int64
		if err := rows.Scan(&n, &t); err != nil {
			return nil, err
		}
		out[n] = time.UnixMilli(t)
	}
	return out, rows.Err()
}

type JobDetail struct {
	JobRow
	Output     string     `json:"output"`
	Depth      int        `json:"depth"`
	ParentID   *int64     `json:"parent_id"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.UnixMilli(n.Int64)
	return &t
}

// GetJob returns one job, or sql.ErrNoRows.
func GetJob(db *sql.DB, id int64) (JobDetail, error) {
	var j JobDetail
	var created int64
	var exit, parent, started, finished sql.NullInt64
	err := db.QueryRow(`SELECT id, rule, state, attempt, created_at, exit_code, output, depth, parent_id, started_at, finished_at
		FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Rule, &j.State, &j.Attempt, &created, &exit, &j.Output, &j.Depth, &parent, &started, &finished)
	if err != nil {
		return j, err
	}
	j.CreatedAt = time.UnixMilli(created)
	if exit.Valid {
		e := int(exit.Int64)
		j.ExitCode = &e
	}
	if parent.Valid {
		j.ParentID = &parent.Int64
	}
	j.StartedAt, j.FinishedAt = timePtr(started), timePtr(finished)
	return j, nil
}

type PendingApproval struct {
	JobID     int64     `json:"job_id"`
	Rule      string    `json:"rule"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func PendingApprovals(db *sql.DB) ([]PendingApproval, error) {
	rows, err := db.Query(`SELECT j.id, j.rule, j.created_at, a.expires_at FROM approvals a JOIN jobs j ON j.id=a.job_id
		WHERE a.decision IS NULL AND j.state='pending_approval' ORDER BY j.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingApproval
	for rows.Next() {
		var p PendingApproval
		var c, e int64
		if err := rows.Scan(&p.JobID, &p.Rule, &c, &e); err != nil {
			return nil, err
		}
		p.CreatedAt, p.ExpiresAt = time.UnixMilli(c), time.UnixMilli(e)
		out = append(out, p)
	}
	return out, rows.Err()
}

type AuditRow struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Event  string    `json:"event"`
	JobID  *int64    `json:"job_id"`
	Detail string    `json:"detail"`
}

func ListAudit(db *sql.DB, limit int) ([]AuditRow, error) {
	return QueryAudit(db, AuditFilter{Limit: limit})
}
