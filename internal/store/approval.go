package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateApproval records the pending approval for a job. Only the token's
// SHA-256 is stored.
func CreateApproval(tx *sql.Tx, jobID int64, tokenHash []byte, expires time.Time) error {
	_, err := tx.Exec(`INSERT INTO approvals(job_id, token_hash, expires_at) VALUES (?,?,?)`, jobID, tokenHash, ms(expires))
	return err
}

var (
	ErrBadToken   = errors.New("invalid approval token")
	ErrNotFound   = errors.New("no such job or approval")
	ErrNotPending = errors.New("approval is not pending") // already decided, expired, or job not pending
)

// DecideApproval approves (job → queued) or denies (job → cancelled) a pending
// job, identified by job id. An empty token skips the token check (operator on
// the host); otherwise it must match the stored hash. A second decision, an
// expired approval or a job that is not pending is an error. Every decision,
// and every bad token, is audited.
func DecideApproval(db *sql.DB, jobID int64, approve bool, by, token string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var hash []byte
	var expires int64
	var decision sql.NullString
	var state string
	err = tx.QueryRow(`SELECT a.token_hash, a.expires_at, a.decision, j.state
		FROM approvals a JOIN jobs j ON j.id=a.job_id WHERE a.job_id=?`, jobID).Scan(&hash, &expires, &decision, &state)
	if err == sql.ErrNoRows {
		return fmt.Errorf("job %d has no approval: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if token != "" {
		sum := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(sum[:], hash) != 1 {
			if err := Audit(tx, now, by, "approval_bad_token", jobID, ""); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return ErrBadToken
		}
	}
	switch {
	case decision.Valid:
		return fmt.Errorf("job %d already %s: %w", jobID, decision.String, ErrNotPending)
	case state != "pending_approval":
		return fmt.Errorf("job %d is %s, not pending approval: %w", jobID, state, ErrNotPending)
	case ms(now) >= expires:
		return fmt.Errorf("approval for job %d has expired: %w", jobID, ErrNotPending)
	}

	dec, event, newState := "denied", "deny", "cancelled"
	if approve {
		dec, event, newState = "approved", "approve", "queued"
	}
	// decision IS NULL keeps the second decision out even under a race.
	r, err := tx.Exec(`UPDATE approvals SET decision=?, decided_by=?, decided_at=? WHERE job_id=? AND decision IS NULL`,
		dec, by, ms(now), jobID)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("job %d already decided: %w", jobID, ErrNotPending)
	}
	if approve {
		_, err = tx.Exec(`UPDATE jobs SET state='queued', run_after=? WHERE id=?`, ms(now), jobID)
	} else {
		_, err = tx.Exec(`UPDATE jobs SET state='cancelled', finished_at=? WHERE id=?`, ms(now), jobID)
	}
	if err != nil {
		return err
	}
	if err := Audit(tx, now, by, event, jobID, newState); err != nil {
		return err
	}
	return tx.Commit()
}

// ExpireApprovals fails pending jobs whose approval expired undecided.
func ExpireApprovals(db *sql.DB, now time.Time) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT j.id FROM jobs j JOIN approvals a ON a.job_id=j.id
		WHERE j.state='pending_approval' AND a.decision IS NULL AND a.expires_at<=? ORDER BY j.id`, ms(now))
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE jobs SET state='failed', finished_at=?, output='approval expired' WHERE id=?`, ms(now), id); err != nil {
			return 0, err
		}
		if err := Audit(tx, now, "system", "approval_expired", id, ""); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}

// JobRow is one line of `jobs ls`.
type JobRow struct {
	ID        int64     `json:"id"`
	Rule      string    `json:"rule"`
	State     string    `json:"state"`
	Attempt   int       `json:"attempt"`
	CreatedAt time.Time `json:"created_at"`
	ExitCode  *int      `json:"exit_code"`
}

// ListJobs returns the newest 200 jobs first; state "" means all.
func ListJobs(db *sql.DB, state string) ([]JobRow, error) { return QueryJobs(db, state, 200) }

// QueryJobs returns the newest jobs first, at most limit; state "" means all.
func QueryJobs(db *sql.DB, state string, limit int) ([]JobRow, error) {
	q := `SELECT id, rule, state, attempt, created_at, exit_code FROM jobs`
	var args []any
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	rows, err := db.Query(q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var j JobRow
		var created int64
		var exit sql.NullInt64
		if err := rows.Scan(&j.ID, &j.Rule, &j.State, &j.Attempt, &created, &exit); err != nil {
			return nil, err
		}
		j.CreatedAt = time.UnixMilli(created)
		if exit.Valid {
			e := int(exit.Int64)
			j.ExitCode = &e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
