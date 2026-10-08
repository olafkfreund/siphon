package store

import (
	"database/sql"
	"time"
)

// ApprovalTTL is how long an approval stays open; it must equal job.approvalTTL.
// The approvals table has no created_at, so creation time is expires_at - ApprovalTTL.
const ApprovalTTL = 24 * time.Hour

// Notification is one outbox row.
type Notification struct {
	ID        int64
	Channel   string
	Event     string
	Key       string
	JobID     int64 // 0 when none
	Source    string
	Title     string
	Body      string
	CreatedAt time.Time
	Attempts  int
	NextAt    time.Time
	SentAt    *time.Time
	State     string // pending | sent | failed | suppressed
	Error     string
}

// ChannelSince returns the channel's baseline, creating it as now on first use.
func ChannelSince(db *sql.DB, name string, now time.Time) (time.Time, error) {
	if _, err := db.Exec(`INSERT OR IGNORE INTO notify_channel(name,since) VALUES (?,?)`, name, ms(now)); err != nil {
		return time.Time{}, err
	}
	var since int64
	err := db.QueryRow(`SELECT since FROM notify_channel WHERE name=?`, name).Scan(&since)
	return time.UnixMilli(since), err
}

// DropChannel forgets a channel's baseline, so adding it again starts fresh.
func DropChannel(db *sql.DB, name string) error {
	_, err := db.Exec(`DELETE FROM notify_channel WHERE name=?`, name)
	return err
}

// OpenApproval is an open approval on a job waiting for a person.
type OpenApproval struct {
	JobAction
	ResumeStep int
	ExpiresAt  time.Time
}

// PendingApprovalsFor lists open, unexpired approvals created at or after since.
func PendingApprovalsFor(db *sql.DB, since, now time.Time) ([]OpenApproval, error) {
	rows, err := db.Query(`SELECT j.id, j.resume_step, a.expires_at FROM approvals a JOIN jobs j ON j.id=a.job_id
		WHERE a.decision IS NULL AND j.state='pending_approval' AND a.expires_at-? >= ? AND a.expires_at > ? ORDER BY j.id`,
		ApprovalTTL.Milliseconds(), ms(since), ms(now))
	if err != nil {
		return nil, err
	}
	type row struct {
		id   int64
		step int
		exp  int64
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.step, &r.exp); err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []OpenApproval{}
	for _, r := range rs {
		js, err := jobActions(db, `id=?`, []any{r.id}, 1)
		if err != nil || len(js) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, OpenApproval{js[0], r.step, time.UnixMilli(r.exp)})
	}
	return out, nil
}

// FailedJobsSince lists jobs that failed after since, oldest first.
func FailedJobsSince(db *sql.DB, since time.Time) ([]JobAction, error) {
	js, err := jobActions(db, `state='failed' AND finished_at > ?`, []any{ms(since)}, 200)
	for i, j := 0, len(js)-1; i < j; i, j = i+1, j-1 {
		js[i], js[j] = js[j], js[i]
	}
	return js, err
}

// FailingSource is a source whose polls have been failing since a point in time.
type FailingSource struct {
	Name  string
	Since time.Time
}

func FailingSources(db *sql.DB) ([]FailingSource, error) {
	rows, err := db.Query(`SELECT source, failing_since FROM source_state WHERE failing_since IS NOT NULL ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailingSource
	for rows.Next() {
		var f FailingSource
		var at int64
		if err := rows.Scan(&f.Name, &at); err != nil {
			return nil, err
		}
		f.Since = time.UnixMilli(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// SentSourceEpisodes are the "source failing" messages sent on the channel
// that have no "recovered" row yet (event source_ok, same key).
func SentSourceEpisodes(db *sql.DB, channel string) ([]Notification, error) {
	return queryNotifications(db, `SELECT `+notifCols+` FROM notifications n
		WHERE n.channel=? AND n.event='source' AND n.state='sent'
		AND NOT EXISTS (SELECT 1 FROM notifications o WHERE o.channel=n.channel AND o.event='source_ok' AND o.key=n.key)
		ORDER BY n.id`, channel)
}

// EnqueueNotification records a message once per (channel, event, key); it
// reports whether this call created the row.
func EnqueueNotification(db *sql.DB, n Notification, now time.Time) (bool, error) {
	var jid any
	if n.JobID != 0 {
		jid = n.JobID
	}
	r, err := db.Exec(`INSERT OR IGNORE INTO notifications(channel,event,key,job_id,source,title,body,created_at,next_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, n.Channel, n.Event, n.Key, jid, n.Source, n.Title, n.Body, ms(now), ms(now))
	if err != nil {
		return false, err
	}
	c, _ := r.RowsAffected()
	return c > 0, nil
}

const notifCols = `n.id, n.channel, n.event, n.key, COALESCE(n.job_id,0), n.source, n.title, n.body,
	n.created_at, n.attempts, n.next_at, n.sent_at, n.state, n.error`

func queryNotifications(db *sql.DB, q string, args ...any) ([]Notification, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var created, next int64
		var sent sql.NullInt64
		if err := rows.Scan(&n.ID, &n.Channel, &n.Event, &n.Key, &n.JobID, &n.Source, &n.Title, &n.Body,
			&created, &n.Attempts, &next, &sent, &n.State, &n.Error); err != nil {
			return nil, err
		}
		n.CreatedAt, n.NextAt = time.UnixMilli(created), time.UnixMilli(next)
		if sent.Valid {
			t := time.UnixMilli(sent.Int64)
			n.SentAt = &t
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DueNotifications are the pending rows whose time has come, oldest first.
func DueNotifications(db *sql.DB, now time.Time, limit int) ([]Notification, error) {
	return queryNotifications(db, `SELECT `+notifCols+` FROM notifications n
		WHERE n.state='pending' AND n.next_at<=? ORDER BY n.next_at, n.id LIMIT ?`, ms(now), limit)
}

func MarkSent(db *sql.DB, id int64, now time.Time) error {
	_, err := db.Exec(`UPDATE notifications SET state='sent', sent_at=?, attempts=attempts+1, error='' WHERE id=?`, ms(now), id)
	return err
}

// MarkRetry keeps the row pending for another try at next. errMsg must be masked.
func MarkRetry(db *sql.DB, id int64, next time.Time, errMsg string) error {
	_, err := db.Exec(`UPDATE notifications SET attempts=attempts+1, next_at=?, error=? WHERE id=?`, ms(next), errMsg, id)
	return err
}

// MarkFailed gives up on a row and audits notify_failed. errMsg must be masked.
func MarkFailed(db *sql.DB, id int64, now time.Time, errMsg string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jid sql.NullInt64
	var channel, event string
	if err := tx.QueryRow(`SELECT job_id, channel, event FROM notifications WHERE id=?`, id).Scan(&jid, &channel, &event); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE notifications SET state='failed', attempts=attempts+1, error=? WHERE id=?`, errMsg, id); err != nil {
		return err
	}
	if err := Audit(tx, now, "notify", "notify_failed", jid.Int64, channel+" "+event+": "+errMsg); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkSuppressed drops a row over the rate cap. It is counted by TakeSuppressed.
func MarkSuppressed(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE notifications SET state='suppressed' WHERE id=?`, id)
	return err
}

// TakeSuppressed returns how many rows of the channel were suppressed since
// the last call, and marks them reported.
func TakeSuppressed(db *sql.DB, channel string) (int, error) {
	r, err := db.Exec(`UPDATE notifications SET reported=1 WHERE channel=? AND state='suppressed' AND reported=0`, channel)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// SentInLastHour counts the channel's delivered messages in the last hour.
func SentInLastHour(db *sql.DB, channel string, now time.Time) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE channel=? AND state='sent' AND sent_at>?`,
		channel, ms(now.Add(-time.Hour))).Scan(&n)
	return n, err
}

// NotificationFilter narrows ListNotifications; zero fields match everything.
type NotificationFilter struct {
	Channel string
	Job     int64
	Limit   int
}

// ListNotifications returns rows newest first.
func ListNotifications(db *sql.DB, f NotificationFilter) ([]Notification, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	return queryNotifications(db, `SELECT `+notifCols+` FROM notifications n
		WHERE (?='' OR n.channel=?) AND (?=0 OR n.job_id=?) ORDER BY n.id DESC LIMIT ?`,
		f.Channel, f.Channel, f.Job, f.Job, f.Limit)
}

// ChannelNames lists the channels that have a baseline row.
func ChannelNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM notify_channel ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
