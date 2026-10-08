package store

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenTwiceIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	for i := 0; i < 2; i++ {
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, tbl := range []string{"jobs", "rule_state", "seen_event", "approvals", "audit", "rule_override", "source_state", "schema_migrations", "rule_error", "connection_check", "notifications", "notify_channel"} {
			var n int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil || n != 1 {
				t.Fatalf("open %d: table %s missing (%v)", i, tbl, err)
			}
		}
		var m int
		s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&m)
		if m != 5 {
			t.Fatalf("migrations recorded: %d", m)
		}
		var fk int
		s.DB.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
		if fk != 1 {
			t.Fatal("foreign_keys off")
		}
		s.Close()
	}
}

func TestHelpers(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.UnixMilli(1_700_000_000_000)
	tx, _ := s.DB.Begin()
	defer tx.Rollback()

	if rs, _ := GetRuleState(tx, "r", "-"); rs.Found {
		t.Fatal("want not found")
	}
	PutRuleState(tx, "r", "-", true, now)
	PutRuleState(tx, "r", "-", false, time.Time{}) // keeps last_fired_at
	rs, err := GetRuleState(tx, "r", "-")
	if err != nil || !rs.Found || rs.LastValue || !rs.LastFired.Equal(now) {
		t.Fatalf("%+v %v", rs, err)
	}

	if n, _ := MarkSeen(tx, "x", "1", now); !n {
		t.Fatal("first should be new")
	}
	if n, _ := MarkSeen(tx, "x", "1", now); n {
		t.Fatal("second should not be new")
	}

	id, err := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}", RunAfter: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}", ParentID: id, Depth: 1}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}", State: "bogus"}, now); err == nil {
		t.Fatal("bad state should violate CHECK")
	}
	if err := Audit(tx, now, "system", "skip", id, "cap"); err != nil {
		t.Fatal(err)
	}
}

func TestFinishJobOnlyRunning(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.UnixMilli(1_700_000_000_000)
	tx, _ := s.DB.Begin()
	id, _ := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}"}, now)
	tx.Commit()
	if err := FinishJob(s.DB, id, "done", 0, "", now); err == nil {
		t.Fatal("queued job must not be finishable")
	}
	if _, ok, err := ClaimJob(s.DB, now); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := FinishJob(s.DB, id, "done", 0, "ok", now); err != nil {
		t.Fatal(err)
	}
	if err := FinishJob(s.DB, id, "failed", 1, "late", now); err == nil {
		t.Fatal("second finish must fail")
	}
}

func TestRequeueRunning(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	now := time.UnixMilli(1_700_000_000_000)
	tx, _ := s.DB.Begin()
	id, _ := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}", State: "running"}, now)
	queued, _ := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}"}, now)
	tx.Commit()

	state := func() (string, int) {
		var st string
		var at int
		s.DB.QueryRow(`SELECT state, attempt FROM jobs WHERE id=?`, id).Scan(&st, &at)
		return st, at
	}
	for i, want := range []struct {
		state string
		att   int
	}{{"queued", 1}, {"queued", 2}, {"failed", 3}} {
		rq, f, err := RequeueRunning(s.DB, now)
		if err != nil || rq+f != 1 {
			t.Fatalf("round %d: %d %d %v", i, rq, f, err)
		}
		if st, at := state(); st != want.state || at != want.att {
			t.Fatalf("round %d: %s/%d", i, st, at)
		}
		if want.state == "queued" { // simulate the next claim
			s.DB.Exec(`UPDATE jobs SET state='running' WHERE id=?`, id)
		}
	}
	if rq, f, _ := RequeueRunning(s.DB, now); rq+f != 0 {
		t.Fatal("failed job must not be touched")
	}
	var st string
	s.DB.QueryRow(`SELECT state FROM jobs WHERE id=?`, queued).Scan(&st)
	if st != "queued" {
		t.Fatal("queued job changed")
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM audit WHERE job_id=?`, id).Scan(&n)
	if n != 3 {
		t.Fatalf("audit rows: %d", n)
	}
}

func TestCleanup(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	now := time.UnixMilli(1_800_000_000_000)
	day := 24 * time.Hour
	mk := func(state string, finished time.Time, parent int64) int64 {
		tx, _ := s.DB.Begin()
		id, err := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}", State: state, ParentID: parent}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Exec(`UPDATE jobs SET finished_at=? WHERE id=?`, ms(finished), id)
		tx.Commit()
		return id
	}
	oldDone := mk("done", now.Add(-31*day), 0)
	freshDone := mk("done", now.Add(-29*day), 0)
	oldQueued := mk("queued", now.Add(-40*day), 0)
	child := mk("done", now.Add(-1*day), oldDone) // parent is purged, child survives
	s.DB.Exec(`INSERT INTO approvals(job_id, token_hash, expires_at) VALUES (?, x'00', 0)`, oldDone)
	tx, _ := s.DB.Begin()
	Audit(tx, now.Add(-31*day), "a", "old", 0, "")
	Audit(tx, now.Add(-1*day), "a", "new", 0, "")
	MarkSeen(tx, "x", "old", now.Add(-8*day))
	MarkSeen(tx, "x", "new", now.Add(-6*day))
	tx.Commit()

	if err := Cleanup(s.DB, now); err != nil {
		t.Fatal(err)
	}
	exists := func(id int64) bool {
		var n int
		s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE id=?`, id).Scan(&n)
		return n == 1
	}
	if exists(oldDone) || !exists(freshDone) || !exists(oldQueued) || !exists(child) {
		t.Fatal("wrong jobs retained")
	}
	count := func(q string) (n int) { s.DB.QueryRow(q).Scan(&n); return }
	if count(`SELECT COUNT(*) FROM audit WHERE event='old'`) != 0 || count(`SELECT COUNT(*) FROM audit WHERE event='new'`) != 1 {
		t.Fatal("audit retention")
	}
	if count(`SELECT COUNT(*) FROM seen_event`) != 1 || count(`SELECT COUNT(*) FROM approvals`) != 0 {
		t.Fatal("seen_event/approvals retention")
	}
}

func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	un, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p); err == nil || !strings.Contains(err.Error(), "another siphon") {
		t.Fatalf("second lock: %v", err)
	}
	un()
	un2, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	un2()
}

func TestConfigOverlay(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	tx, _ := s.DB.Begin()
	if err := PutConfigItem(tx, "rules", "a", "name: a", false, now); err != nil {
		t.Fatal(err)
	}
	PutConfigItem(tx, "sources", "s", "", true, now)
	PutConfigItem(tx, "rules", "a", "name: a2", false, now) // upsert
	id1, err := AddRevision(tx, now, "me", "one", `[]`, "d1")
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := AddRevision(tx, now, "me", "two", `[]`, "d2")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	items, _ := ConfigItems(s.DB)
	if len(items) != 2 || items[0].YAML != "name: a2" || !items[1].Deleted {
		t.Fatalf("items %+v", items)
	}
	tx, _ = s.DB.Begin()
	DeleteConfigItem(tx, "rules", "a")
	tx.Commit()
	if items, _ = ConfigItems(s.DB); len(items) != 1 {
		t.Fatalf("after delete %+v", items)
	}
	revs, _ := Revisions(s.DB, 10)
	if len(revs) != 2 || revs[0].ID != id2 || id2 <= id1 {
		t.Fatalf("revs %+v", revs)
	}
	if r, err := Revision(s.DB, id1); err != nil || r.Summary != "one" || r.Diff != "d1" {
		t.Fatalf("rev %+v %v", r, err)
	}
	if r, ok, _ := LatestRevision(s.DB); !ok || r.ID != id2 {
		t.Fatalf("latest %+v", r)
	}
	s2, _ := Open(":memory:")
	defer s2.Close()
	if _, ok, err := LatestRevision(s2.DB); ok || err != nil {
		t.Fatalf("empty latest %v %v", ok, err)
	}
}

func TestSetSourceEvent(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	get := func() (j string) {
		s.DB.QueryRow(`SELECT json FROM source_state WHERE source='x'`).Scan(&j)
		return
	}
	ev := map[string]any{"n": 1, "Api_Key": "k", "a": []any{map[string]any{"PASSWORD": "p", "ok": "v"}}}
	if err := SetSourceEvent(s.DB, "x", ev); err != nil {
		t.Fatal(err)
	}
	if want := `{"Api_Key":"[redacted]","a":[{"PASSWORD":"[redacted]","ok":"v"}],"n":1}`; get() != want {
		t.Fatalf("got %s", get())
	}
	// poll state survives an event write, and vice versa
	PutSourceState(s.DB, "x", time.Now(), "boom")
	if get() == "" {
		t.Fatal("PutSourceState clobbered json")
	}
	SetSourceEvent(s.DB, "x", map[string]string{"big": strings.Repeat("a", 70<<10)})
	if get() != `{"_truncated":true}` {
		t.Fatalf("cap: %.40s", get())
	}
}

func TestMigrationOnExistingDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.db")
	s, _ := Open(p)
	s.DB.Exec(`DELETE FROM schema_migrations WHERE version LIKE '0002%'`)
	s.DB.Exec(`DROP TABLE config_item`)
	s.DB.Exec(`DROP TABLE config_revision`)
	s.Close()
	s, err := Open(p) // 0001 already applied; 0002 applies on top
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := ConfigItems(s.DB); err != nil {
		t.Fatal(err)
	}
}

// 0003 applies on top of a database that only has 0001 and 0002, keeping its rows.
func TestMigration0003OnExistingDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	q := url.Values{"_pragma": {"foreign_keys(1)"}}
	db, err := sql.Open("sqlite", "file:"+p+"?"+q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`)
	for _, n := range []string{"0001_init.sql", "0002_config_overlay.sql"} {
		b, _ := migrationFS.ReadFile("migrations/" + n)
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
		db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, n)
	}
	db.Exec(`INSERT INTO source_state(source,json,last_error) VALUES ('old','{"a":1}','boom')`)
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if j, _ := SourceEvent(s.DB, "old"); j != `{"a":1}` {
		t.Fatalf("old row lost: %q", j)
	}
	d, err := SourceDiagnostics(s.DB, "old")
	if err != nil || d.EventAt != nil || d.Reject != "" {
		t.Fatalf("%+v %v", d, err)
	}
	if err := SetSourceReject(s.DB, "old", time.Now(), 401, "bad"); err != nil {
		t.Fatal(err)
	}
	if d, _ := SourceDiagnostics(s.DB, "old"); d.Reject != "bad (401)" || d.RejectAt == nil {
		t.Fatalf("%+v", d)
	}
}

// 0004 applies on top of a database from before it, keeping its rows.
func TestMigration0004OnExistingDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`)
	for _, n := range []string{"0001_init.sql", "0002_config_overlay.sql", "0003_diagnostics.sql"} {
		b, _ := migrationFS.ReadFile("migrations/" + n)
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
		db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, n)
	}
	db.Exec(`INSERT INTO source_state(source,json,last_error) VALUES ('old','{"a":1}','')`)
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if j, _ := SourceEvent(s.DB, "old"); j != `{"a":1}` {
		t.Fatalf("old row lost: %q", j)
	}
	if c, err := GetConnectionCheck(s.DB, "x"); c != nil || err != nil {
		t.Fatalf("%v %v", c, err)
	}
	at := time.Unix(1700000000, 0)
	SetConnectionCheck(s.DB, ConnectionCheck{"x", at, true, "olaf"})
	SetConnectionCheck(s.DB, ConnectionCheck{"x", at.Add(time.Minute), false, "refused"})
	if c, _ := GetConnectionCheck(s.DB, "x"); c == nil || c.OK || c.Detail != "refused" || !c.At.Equal(at.Add(time.Minute)) {
		t.Fatalf("%+v", c)
	}
}

// 0005 applies on top of a 0004 database, keeping its rows and adding failing_since.
func TestMigration0005OnExistingDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`)
	for _, n := range []string{"0001_init.sql", "0002_config_overlay.sql", "0003_diagnostics.sql", "0004_connection_check.sql"} {
		b, _ := migrationFS.ReadFile("migrations/" + n)
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
		db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, n)
	}
	db.Exec(`INSERT INTO source_state(source,last_poll_at,last_error) VALUES ('old',1,'boom')`)
	db.Close()
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fs, err := FailingSources(s.DB); err != nil || len(fs) != 0 {
		t.Fatalf("old row should not be failing: %v %v", fs, err)
	}
	now := time.UnixMilli(5000)
	PutSourceState(s.DB, "old", now, "boom") // first failure seen by this version starts the run
	if fs, _ := FailingSources(s.DB); len(fs) != 1 || !fs[0].Since.Equal(now) {
		t.Fatalf("%+v", fs)
	}
}

func TestFailingSince(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	t0 := time.UnixMilli(1000)
	since := func() []FailingSource { f, _ := FailingSources(s.DB); return f }
	PutSourceState(s.DB, "a", t0, "")
	if len(since()) != 0 {
		t.Fatal("healthy source is failing")
	}
	PutSourceState(s.DB, "a", t0.Add(time.Minute), "x")
	PutSourceState(s.DB, "a", t0.Add(2*time.Minute), "y")
	if f := since(); len(f) != 1 || !f[0].Since.Equal(t0.Add(time.Minute)) {
		t.Fatalf("run start moved: %+v", f)
	}
	PutSourceState(s.DB, "a", t0.Add(3*time.Minute), "")
	if len(since()) != 0 {
		t.Fatal("recovery did not clear")
	}
	PutSourceState(s.DB, "a", t0.Add(4*time.Minute), "z")
	if f := since(); len(f) != 1 || !f[0].Since.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("new run: %+v", f)
	}
	PutSourceState(s.DB, "b", t0, "first") // insert while failing
	if len(since()) != 2 {
		t.Fatal("insert-as-failing missed")
	}
}

func TestNotificationOutbox(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	db := s.DB
	t0 := time.UnixMilli(1_000_000)
	if a, _ := ChannelSince(db, "c", t0); !a.Equal(t0) {
		t.Fatal(a)
	}
	if a, _ := ChannelSince(db, "c", t0.Add(time.Hour)); !a.Equal(t0) {
		t.Fatalf("baseline moved: %v", a)
	}
	DropChannel(db, "c")
	if a, _ := ChannelSince(db, "c", t0.Add(time.Hour)); !a.Equal(t0.Add(time.Hour)) {
		t.Fatalf("not fresh: %v", a)
	}

	n := Notification{Channel: "c", Event: "failed", Key: "job:1", JobID: 0, Title: "t", Body: "b"}
	if ok, err := EnqueueNotification(db, n, t0); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := EnqueueNotification(db, n, t0); ok {
		t.Fatal("duplicate recorded")
	}
	n2 := n
	n2.Key = "job:2"
	EnqueueNotification(db, n2, t0.Add(time.Second))
	due, _ := DueNotifications(db, t0.Add(time.Second), 10)
	if len(due) != 2 || due[0].Key != "job:1" {
		t.Fatalf("%+v", due)
	}
	if d, _ := DueNotifications(db, t0.Add(-time.Second), 10); len(d) != 0 {
		t.Fatal("not yet due")
	}
	MarkRetry(db, due[0].ID, t0.Add(time.Hour), "boom")
	if d, _ := DueNotifications(db, t0.Add(time.Minute), 10); len(d) != 1 || d[0].Key != "job:2" {
		t.Fatalf("%+v", d)
	}
	MarkSent(db, due[1].ID, t0.Add(time.Minute))
	if c, _ := SentInLastHour(db, "c", t0.Add(2*time.Minute)); c != 1 {
		t.Fatal(c)
	}
	if c, _ := SentInLastHour(db, "c", t0.Add(2*time.Hour)); c != 0 {
		t.Fatal(c)
	}
	if err := MarkFailed(db, due[0].ID, t0, "gave up"); err != nil {
		t.Fatal(err)
	}
	var ev string
	if db.QueryRow(`SELECT event FROM audit WHERE event='notify_failed'`).Scan(&ev) != nil {
		t.Fatal("no audit row")
	}
	l, _ := ListNotifications(db, NotificationFilter{Channel: "c"})
	if len(l) != 2 || l[0].Key != "job:2" || l[0].State != "sent" || l[0].SentAt == nil || l[1].State != "failed" || l[1].Attempts != 2 {
		t.Fatalf("%+v", l)
	}

	n3 := n
	n3.Key = "job:3"
	EnqueueNotification(db, n3, t0)
	d, _ := DueNotifications(db, t0, 10)
	MarkSuppressed(db, d[0].ID)
	if c, _ := TakeSuppressed(db, "c"); c != 1 {
		t.Fatal(c)
	}
	if c, _ := TakeSuppressed(db, "c"); c != 0 {
		t.Fatal("suppressed counted twice")
	}
}

func TestNotifyScans(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	db := s.DB
	t0 := time.UnixMilli(10_000_000)
	act := `{"action":{"Agent":"rev"},"agents":{"rev":{"Kind":"claude"}}}`
	mk := func(state string, finished any, step int) int64 {
		r, err := db.Exec(`INSERT INTO jobs(rule,action_json,state,finished_at,resume_step,created_at) VALUES ('r',?,?,?,?,?)`, act, state, finished, step, ms(t0))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	appr := func(job int64, created time.Time, decision any) {
		db.Exec(`INSERT INTO approvals(job_id,token_hash,expires_at,decision) VALUES (?,?,?,?)`, job, []byte("h"), ms(created.Add(ApprovalTTL)), decision)
	}
	old, fresh, decided := mk("pending_approval", nil, 0), mk("pending_approval", nil, 2), mk("pending_approval", nil, 0)
	appr(old, t0.Add(-time.Hour), nil)
	appr(fresh, t0.Add(time.Hour), nil)
	appr(decided, t0.Add(time.Hour), "approved")
	ps, err := PendingApprovalsFor(db, t0, t0)
	if err != nil || len(ps) != 1 || ps[0].ID != fresh || ps[0].ResumeStep != 2 || ps[0].Kind != "agent" || !ps[0].ExpiresAt.Equal(t0.Add(time.Hour+ApprovalTTL)) {
		t.Fatalf("%+v %v", ps, err)
	}
	exp := mk("pending_approval", nil, 0)
	appr(exp, t0.Add(-ApprovalTTL-time.Hour), nil)
	if ps, _ := PendingApprovalsFor(db, t0.Add(-48*time.Hour), t0); len(ps) != 2 || ps[0].ID == exp || ps[1].ID == exp {
		t.Fatalf("expired approval listed: %+v", ps)
	}
	a, b := mk("failed", ms(t0.Add(-time.Minute)), 0), mk("failed", ms(t0.Add(time.Minute)), 0)
	mk("done", ms(t0.Add(time.Minute)), 0)
	fj, _ := FailedJobsSince(db, t0)
	if len(fj) != 1 || fj[0].ID != b || a == b {
		t.Fatalf("%+v", fj)
	}

	// Source episodes: sent "failing" rows without a "recovered" row.
	for _, k := range []string{"src:a:1", "src:b:2"} {
		EnqueueNotification(db, Notification{Channel: "c", Event: "source", Key: k, Source: k[4:5], Title: "t", Body: "b"}, t0)
	}
	due, _ := DueNotifications(db, t0, 10)
	for _, d := range due {
		MarkSent(db, d.ID, t0)
	}
	EnqueueNotification(db, Notification{Channel: "c", Event: "source_ok", Key: "src:a:1", Source: "a", Title: "t", Body: "b"}, t0)
	if eps, _ := SentSourceEpisodes(db, "c"); len(eps) != 1 || eps[0].Key != "src:b:2" || eps[0].Source != "b" {
		t.Fatalf("%+v", eps)
	}
}

func TestCleanupFreesJobIDsForNotifications(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	now := time.UnixMilli(1_800_000_000_000)
	old := now.Add(-31 * 24 * time.Hour)
	mk := func() int64 {
		tx, _ := s.DB.Begin()
		id, _ := InsertJob(tx, Job{Rule: "r", ActionJSON: "{}", State: "failed"}, old)
		tx.Exec(`UPDATE jobs SET finished_at=? WHERE id=?`, ms(old), id)
		tx.Commit()
		return id
	}
	id := mk()
	n := Notification{Channel: "c", Event: "failed", Key: "job:" + strconv.FormatInt(id, 10), JobID: id, Title: "t", Body: "b"}
	if c, _ := EnqueueNotification(s.DB, n, old); !c {
		t.Fatal("first enqueue")
	}
	MarkSent(s.DB, 1, old)
	// job-less rows: a sent source episode with no recovery is kept; an old sent one that recovered goes.
	for _, r := range []Notification{{Event: "source", Key: "a"}, {Event: "source", Key: "b"}, {Event: "source_ok", Key: "b"}, {Event: "test", Key: "z"}} {
		r.Channel, r.Title, r.Body = "c", "t", "b"
		EnqueueNotification(s.DB, r, old)
	}
	s.DB.Exec(`UPDATE notifications SET state='sent' WHERE job_id IS NULL`)
	if err := Cleanup(s.DB, now); err != nil {
		t.Fatal(err)
	}
	if id2 := mk(); id2 != id {
		t.Skipf("ids not reused (%d,%d)", id, id2)
	}
	if c, err := EnqueueNotification(s.DB, n, now); err != nil || !c {
		t.Fatalf("reused id's notification dropped: %v %v", c, err)
	}
	var keys string
	s.DB.QueryRow(`SELECT group_concat(key) FROM notifications WHERE job_id IS NULL`).Scan(&keys)
	if keys != "a" {
		t.Fatalf("job-less rows left: %q", keys)
	}
}

func TestSuppressedReportLeavesErrorEmpty(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	now := time.UnixMilli(1_800_000_000_000)
	EnqueueNotification(s.DB, Notification{Channel: "c", Event: "failed", Key: "k", Title: "t", Body: "b"}, now)
	MarkSuppressed(s.DB, 1)
	if n, _ := TakeSuppressed(s.DB, "c"); n != 1 {
		t.Fatal(n)
	}
	if n, _ := TakeSuppressed(s.DB, "c"); n != 0 {
		t.Fatal("counted twice")
	}
	if l, _ := ListNotifications(s.DB, NotificationFilter{}); l[0].Error != "" {
		t.Fatalf("error set: %q", l[0].Error)
	}
}
