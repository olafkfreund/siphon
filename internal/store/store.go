// Package store is the SQLite layer. All hand-written queries live here.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"os"
	"sort"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // driver name "sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	DB *sql.DB // use DB.Begin() for transactions
}

// Open opens (creating if needed) the DB at path and applies migrations.
// ":memory:" gives a private in-memory DB.
func Open(path string) (*Store, error) {
	q := url.Values{"_pragma": {"journal_mode(WAL)", "busy_timeout(5000)", "foreign_keys(1)"}}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// ponytail: single connection serialises everything; also required for :memory:.
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	files, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		var done int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, n).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + n)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", n, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, n); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// RuleState is the per-(rule,key) edge/cooldown memory.
type RuleState struct {
	Found     bool
	LastValue bool
	LastFired time.Time // zero if never
}

func GetRuleState(tx *sql.Tx, rule, key string) (RuleState, error) {
	var v int
	var fired sql.NullInt64
	err := tx.QueryRow(`SELECT last_value, last_fired_at FROM rule_state WHERE rule=? AND key=?`, rule, key).Scan(&v, &fired)
	if err == sql.ErrNoRows {
		return RuleState{}, nil
	}
	if err != nil {
		return RuleState{}, err
	}
	rs := RuleState{Found: true, LastValue: v != 0}
	if fired.Valid {
		rs.LastFired = time.UnixMilli(fired.Int64)
	}
	return rs, nil
}

// PutRuleState upserts the value; a non-zero firedAt also updates last_fired_at,
// a zero one keeps the old value.
func PutRuleState(tx *sql.Tx, rule, key string, value bool, firedAt time.Time) error {
	var fired any
	if !firedAt.IsZero() {
		fired = ms(firedAt)
	}
	_, err := tx.Exec(`INSERT INTO rule_state(rule,key,last_value,last_fired_at) VALUES (?,?,?,?)
		ON CONFLICT(rule,key) DO UPDATE SET last_value=excluded.last_value,
		last_fired_at=COALESCE(excluded.last_fired_at, last_fired_at)`, rule, key, value, fired)
	return err
}

// MarkSeen records (scope,id); isNew is false if it was already seen.
func MarkSeen(tx *sql.Tx, scope, id string, now time.Time) (isNew bool, err error) {
	r, err := tx.Exec(`INSERT OR IGNORE INTO seen_event(scope,id,seen_at) VALUES (?,?,?)`, scope, id, ms(now))
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// Job is the subset of columns needed to enqueue.
type Job struct {
	Rule       string
	ActionJSON string
	State      string // default queued
	RunAfter   time.Time
	ParentID   int64 // 0 = none
	Depth      int
}

func InsertJob(tx *sql.Tx, j Job, now time.Time) (int64, error) {
	if j.State == "" {
		j.State = "queued"
	}
	var parent any
	if j.ParentID != 0 {
		parent = j.ParentID
	}
	r, err := tx.Exec(`INSERT INTO jobs(rule,action_json,state,run_after,parent_id,depth,created_at) VALUES (?,?,?,?,?,?,?)`,
		j.Rule, j.ActionJSON, j.State, ms(j.RunAfter), parent, j.Depth, ms(now))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func Audit(tx *sql.Tx, now time.Time, actor, event string, jobID int64, detail string) error {
	var jid any
	if jobID != 0 {
		jid = jobID
	}
	_, err := tx.Exec(`INSERT INTO audit(at,actor,event,job_id,detail) VALUES (?,?,?,?,?)`, ms(now), actor, event, jid, detail)
	return err
}

// RuleLastFired is the latest fire time of any key of rule (zero if never).
func RuleLastFired(tx *sql.Tx, rule string) (time.Time, error) {
	var t sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(last_fired_at) FROM rule_state WHERE rule=?`, rule).Scan(&t); err != nil || !t.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(t.Int64), nil
}

// QueuedJob is a claimed job ready to run.
type QueuedJob struct {
	ID         int64
	Rule       string
	ActionJSON string
	Attempt    int
	Depth      int
	Output     string // routine progress JSON when resuming
	ResumeStep int
}

// ClaimJob marks the oldest runnable queued job as running and returns it.
// ok is false when nothing is runnable. Single process, so no SKIP LOCKED.
func ClaimJob(db *sql.DB, now time.Time) (j QueuedJob, ok bool, err error) {
	err = db.QueryRow(`UPDATE jobs SET state='running', started_at=?
		WHERE id=(SELECT id FROM jobs WHERE state='queued' AND run_after<=? ORDER BY id LIMIT 1)
		RETURNING id, rule, action_json, attempt, depth, output, resume_step`, ms(now), ms(now)).
		Scan(&j.ID, &j.Rule, &j.ActionJSON, &j.Attempt, &j.Depth, &j.Output, &j.ResumeStep)
	if err == sql.ErrNoRows {
		return j, false, nil
	}
	return j, err == nil, err
}

// FinishJob records the outcome of a running job; it errors if the job is not running.
func FinishJob(db *sql.DB, id int64, state string, exit int, output string, now time.Time) error {
	r, err := db.Exec(`UPDATE jobs SET state=?, exit_code=?, output=?, finished_at=? WHERE id=? AND state='running'`,
		state, exit, output, ms(now), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("finish job %d: not running", id)
	}
	return nil
}

// PutSourceState records the last poll time and error ("" on success).
// failing_since marks where the current run of failures began, and clears on success.
func PutSourceState(db *sql.DB, source string, now time.Time, pollErr string) error {
	_, err := db.Exec(`INSERT INTO source_state(source,last_poll_at,last_error,failing_since)
		VALUES (?,?,?,CASE WHEN ?='' THEN NULL ELSE ? END)
		ON CONFLICT(source) DO UPDATE SET last_poll_at=excluded.last_poll_at, last_error=excluded.last_error,
		failing_since=CASE WHEN excluded.last_error='' THEN NULL ELSE COALESCE(source_state.failing_since, excluded.last_poll_at) END`,
		source, ms(now), pollErr, pollErr, ms(now))
	return err
}

// MaxAttempts is how many times an interrupted job is retried before it fails.
const MaxAttempts = 3

// RequeueRunning is the startup recovery: every `running` job was interrupted,
// so it goes back to `queued` with attempt+1, or fails once attempt reaches MaxAttempts.
// Each decision is audited.
func RequeueRunning(db *sql.DB, now time.Time) (requeued, failed int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	type row struct{ id, attempt int64 }
	var jobs []row
	rows, err := tx.Query(`SELECT id, attempt FROM jobs WHERE state='running' ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.attempt); err != nil {
			rows.Close()
			return 0, 0, err
		}
		jobs = append(jobs, r)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	rows.Close()
	for _, j := range jobs {
		n := j.attempt + 1
		if n >= MaxAttempts {
			_, err = tx.Exec(`UPDATE jobs SET state='failed', attempt=?, finished_at=?,
				output=output || 'interrupted too many times' WHERE id=?`, n, ms(now), j.id)
			failed++
			if err == nil {
				err = Audit(tx, now, "system", "requeue_failed", j.id, fmt.Sprintf("attempt %d", n))
			}
		} else {
			_, err = tx.Exec(`UPDATE jobs SET state='queued', attempt=?, started_at=NULL, run_after=? WHERE id=?`, n, ms(now), j.id)
			requeued++
			if err == nil {
				err = Audit(tx, now, "system", "requeue", j.id, fmt.Sprintf("attempt %d", n))
			}
		}
		if err != nil {
			return 0, 0, err
		}
	}
	return requeued, failed, tx.Commit()
}

const (
	jobRetention  = 30 * 24 * time.Hour
	seenRetention = 7 * 24 * time.Hour
)

// Cleanup deletes finished jobs and audit rows older than 30 days and
// seen_event rows older than 7 days.
func Cleanup(db *sql.DB, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old := `SELECT id FROM jobs WHERE state IN ('done','failed','cancelled') AND finished_at < ?`
	cut := ms(now.Add(-jobRetention))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE jobs SET parent_id=NULL WHERE parent_id IN (` + old + `)`, []any{cut}},
		{`DELETE FROM approvals WHERE job_id IN (` + old + `)`, []any{cut}},
		// Job ids are reused once the newest is deleted, so a job's notifications go with it
		// (else INSERT OR IGNORE would drop the next job's message). Old job-less rows go too,
		// except a "source failing" row still awaiting its "recovered" row.
		{`DELETE FROM notifications WHERE job_id IN (` + old + `)`, []any{cut}},
		{`DELETE FROM notifications WHERE job_id IS NULL AND event='source' AND created_at < ? AND (state!='sent' OR EXISTS
			(SELECT 1 FROM notifications o WHERE o.channel=notifications.channel AND o.event='source_ok' AND o.key=notifications.key))`, []any{cut}},
		{`DELETE FROM notifications WHERE job_id IS NULL AND state!='pending' AND created_at < ? AND event!='source'`, []any{cut}},
		{`DELETE FROM jobs WHERE id IN (` + old + `)`, []any{cut}},
		{`DELETE FROM audit WHERE at < ?`, []any{cut}},
		{`DELETE FROM seen_event WHERE seen_at < ?`, []any{ms(now.Add(-seenRetention))}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountAgentJobsSince counts agent jobs created at or after since (daily cap):
// agent actions, and routines whose payload carries "agent": true.
// A routine counts once, however many agent steps it has.
func CountAgentJobsSince(tx *sql.Tx, since time.Time) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT count(*) FROM jobs WHERE created_at >= ?
		AND (COALESCE(json_extract(action_json, '$.action.Agent'), '') != ''
		  OR COALESCE(json_extract(action_json, '$.agent'), 0) = 1)`, ms(since)).Scan(&n)
	return n, err
}

// SaveProgress records routine progress (output JSON, next step) for a running job,
// so a crash or restart resumes without re-running finished steps.
func SaveProgress(db *sql.DB, id int64, nextStep int, output string) error {
	r, err := db.Exec(`UPDATE jobs SET resume_step=?, output=? WHERE id=? AND state='running'`, nextStep, output, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("save progress for job %d: not running", id)
	}
	return nil
}

// RequeueJob sends a running routine job back to queued to retry its current
// step at runAfter, keeping progress; the worker loop honours run_after.
func RequeueJob(db *sql.DB, id int64, step int, output string, runAfter time.Time) error {
	r, err := db.Exec(`UPDATE jobs SET state='queued', started_at=NULL, resume_step=?, output=?, run_after=?
		WHERE id=? AND state='running'`, step, output, ms(runAfter), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("requeue job %d: not running", id)
	}
	return nil
}

// PauseJob moves a running routine job to pending_approval at step, keeping its
// progress. The caller creates the approvals row in the same tx.
func PauseJob(tx *sql.Tx, id int64, step int, output string) error {
	r, err := tx.Exec(`UPDATE jobs SET state='pending_approval', resume_step=?, output=? WHERE id=? AND state='running'`, step, output, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("pause job %d: not running", id)
	}
	return nil
}

// Lock takes an exclusive, non-blocking flock on <dbPath>.lock so two
// daemons/run-once processes never share one DB. The lock dies with the process.
func Lock(dbPath string) (unlock func(), err error) {
	if dbPath == ":memory:" {
		return func() {}, nil
	}
	f, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another siphon (serve or run-once) holds %s", dbPath)
	}
	return func() { f.Close() }, nil // closing releases the flock
}
