package store

import (
	"database/sql"
	"time"
)

// Dashboard is the portal overview: counts since a point in time plus the
// jobs that need a person (pending approvals, recent failures).
type Dashboard struct {
	Counts       map[string]int // state -> jobs created since
	AgentRuns    int            // agent jobs since (the daily-cap count)
	Pending      []JobAction
	RecentFailed []JobAction
}

// JobAction is a job with a one-line summary of what it runs.
type JobAction struct {
	JobRow
	Kind   string // agent | cmd | unit | routine
	Target string // agent name, argv[0], unit or routine name
}

func GetDashboard(db *sql.DB, since time.Time) (Dashboard, error) {
	d := Dashboard{Counts: map[string]int{}}
	rows, err := db.Query(`SELECT state, count(*) FROM jobs WHERE created_at >= ? GROUP BY state`, ms(since))
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return d, err
		}
		d.Counts[s] = n
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return d, err
	}
	d.AgentRuns, err = CountAgentJobsSince(tx, since)
	tx.Rollback()
	if err != nil {
		return d, err
	}
	if d.Pending, err = jobActions(db, `state='pending_approval'`, 0, 20); err != nil {
		return d, err
	}
	d.RecentFailed, err = jobActions(db, `state='failed' AND created_at >= ?`, ms(since), 5)
	return d, err
}

func jobActions(db *sql.DB, where string, since int64, limit int) ([]JobAction, error) {
	q := `SELECT id, rule, state, attempt, created_at, exit_code,
		COALESCE(json_extract(action_json,'$.action.Agent'),''),
		COALESCE(json_extract(action_json,'$.action.Cmd[0]'),''),
		COALESCE(json_extract(action_json,'$.action.Unit'),''),
		COALESCE(json_extract(action_json,'$.action.Routine'),'')
		FROM jobs WHERE ` + where + ` ORDER BY id DESC LIMIT ?`
	args := []any{limit}
	if since != 0 {
		args = []any{since, limit}
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobAction{}
	for rows.Next() {
		var j JobAction
		var created int64
		var exit sql.NullInt64
		var agent, cmd, unit, routine string
		if err := rows.Scan(&j.ID, &j.Rule, &j.State, &j.Attempt, &created, &exit, &agent, &cmd, &unit, &routine); err != nil {
			return nil, err
		}
		j.CreatedAt = time.UnixMilli(created)
		if exit.Valid {
			e := int(exit.Int64)
			j.ExitCode = &e
		}
		switch {
		case agent != "":
			j.Kind, j.Target = "agent", agent
		case routine != "":
			j.Kind, j.Target = "routine", routine
		case unit != "":
			j.Kind, j.Target = "unit", unit
		case cmd != "":
			j.Kind, j.Target = "cmd", cmd
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
