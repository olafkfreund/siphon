package store

import (
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// Diagnostics for `why`: the last rule evaluation error, the last webhook
// rejection and when a source last produced an event. Status codes and short
// reasons only; never a body, signature or token.

const maxDiagText = 300

func clip(s string) string {
	if len(s) > maxDiagText {
		return s[:maxDiagText] + "…"
	}
	return s
}

// PutRuleError records a rule's last evaluation error (one row per rule).
func PutRuleError(tx *sql.Tx, rule string, now time.Time, msg string) error {
	_, err := tx.Exec(`INSERT INTO rule_error(rule,at,error) VALUES (?,?,?)
		ON CONFLICT(rule) DO UPDATE SET at=excluded.at, error=excluded.error`, rule, ms(now), clip(msg))
	return err
}

// ClearRuleError forgets a rule's error after a clean evaluation.
func ClearRuleError(tx *sql.Tx, rule string) error {
	_, err := tx.Exec(`DELETE FROM rule_error WHERE rule=?`, rule)
	return err
}

// RuleError is the rule's last evaluation error ("" if none).
func RuleError(db *sql.DB, rule string) (msg string, at time.Time, err error) {
	var t int64
	err = db.QueryRow(`SELECT at, error FROM rule_error WHERE rule=?`, rule).Scan(&t, &msg)
	if err == sql.ErrNoRows {
		return "", time.Time{}, nil
	}
	return msg, time.UnixMilli(t), err
}

// SetSourceReject records a refused webhook delivery: status and reason only.
func SetSourceReject(db *sql.DB, source string, now time.Time, status int, reason string) error {
	_, err := db.Exec(`INSERT INTO source_state(source,last_reject_at,last_reject) VALUES (?,?,?)
		ON CONFLICT(source) DO UPDATE SET last_reject_at=excluded.last_reject_at, last_reject=excluded.last_reject`,
		source, ms(now), clip(reason)+" ("+strconv.Itoa(status)+")")
	return err
}

// SourceDiag is what a source has stored beyond its poll state.
type SourceDiag struct {
	EventAt, RejectAt *time.Time
	Reject            string
}

func SourceDiagnostics(db *sql.DB, source string) (d SourceDiag, err error) {
	var ev, rj sql.NullInt64
	err = db.QueryRow(`SELECT event_at, last_reject_at, last_reject FROM source_state WHERE source=?`, source).Scan(&ev, &rj, &d.Reject)
	if err == sql.ErrNoRows {
		return d, nil
	}
	d.EventAt, d.RejectAt = timePtr(ev), timePtr(rj)
	return d, err
}

// RuleStateRow is one edge key's remembered value.
type RuleStateRow struct {
	Key         string     `json:"key"`
	LastValue   bool       `json:"last_value"`
	LastFiredAt *time.Time `json:"last_fired_at"`
}

func RuleStates(db *sql.DB, rule string) ([]RuleStateRow, error) {
	rows, err := db.Query(`SELECT key, last_value, last_fired_at FROM rule_state WHERE rule=? ORDER BY key`, rule)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuleStateRow
	for rows.Next() {
		var r RuleStateRow
		var v int
		var at sql.NullInt64
		if err := rows.Scan(&r.Key, &v, &at); err != nil {
			return nil, err
		}
		r.LastValue, r.LastFiredAt = v != 0, timePtr(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuditFilter narrows the audit list. Rule matches the rule's own rows
// (actor rule:<name>) and rows of its jobs; zero values match everything.
type AuditFilter struct {
	Rule, Event string
	Since       time.Time
	Limit       int
}

func QueryAudit(db *sql.DB, f AuditFilter) ([]AuditRow, error) {
	q := `SELECT a.id, a.at, a.actor, a.event, a.job_id, a.detail FROM audit a LEFT JOIN jobs j ON j.id=a.job_id`
	var where []string
	var args []any
	if f.Rule != "" {
		where, args = append(where, `(a.actor=? OR j.rule=?)`), append(args, "rule:"+f.Rule, f.Rule)
	}
	if f.Event != "" {
		where, args = append(where, `a.event=?`), append(args, f.Event)
	}
	if !f.Since.IsZero() {
		where, args = append(where, `a.at>=?`), append(args, ms(f.Since))
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := db.Query(q+` ORDER BY a.id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		var at int64
		var job sql.NullInt64
		if err := rows.Scan(&a.ID, &at, &a.Actor, &a.Event, &job, &a.Detail); err != nil {
			return nil, err
		}
		a.At = time.UnixMilli(at)
		if job.Valid {
			a.JobID = &job.Int64
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
