package store

import (
	"path/filepath"
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
