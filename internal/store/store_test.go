package store

import (
	"path/filepath"
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
		for _, tbl := range []string{"jobs", "rule_state", "seen_event", "approvals", "audit", "rule_override", "source_state", "schema_migrations"} {
			var n int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil || n != 1 {
				t.Fatalf("open %d: table %s missing (%v)", i, tbl, err)
			}
		}
		var m int
		s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&m)
		if m != 1 {
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
