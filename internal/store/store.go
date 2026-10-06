// Package store is the SQLite layer. All hand-written queries live here.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"sort"
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
}

// ClaimJob marks the oldest runnable queued job as running and returns it.
// ok is false when nothing is runnable. Single process, so no SKIP LOCKED.
func ClaimJob(db *sql.DB, now time.Time) (j QueuedJob, ok bool, err error) {
	err = db.QueryRow(`UPDATE jobs SET state='running', started_at=?
		WHERE id=(SELECT id FROM jobs WHERE state='queued' AND run_after<=? ORDER BY id LIMIT 1)
		RETURNING id, rule, action_json, attempt, depth`, ms(now), ms(now)).
		Scan(&j.ID, &j.Rule, &j.ActionJSON, &j.Attempt, &j.Depth)
	if err == sql.ErrNoRows {
		return j, false, nil
	}
	return j, err == nil, err
}

// FinishJob records the outcome of a running job.
func FinishJob(db *sql.DB, id int64, state string, exit int, output string, now time.Time) error {
	_, err := db.Exec(`UPDATE jobs SET state=?, exit_code=?, output=?, finished_at=? WHERE id=?`,
		state, exit, output, ms(now), id)
	return err
}

// PutSourceState records the last poll time and error ("" on success).
func PutSourceState(db *sql.DB, source string, now time.Time, pollErr string) error {
	_, err := db.Exec(`INSERT INTO source_state(source,last_poll_at,last_error) VALUES (?,?,?)
		ON CONFLICT(source) DO UPDATE SET last_poll_at=excluded.last_poll_at, last_error=excluded.last_error`,
		source, ms(now), pollErr)
	return err
}
