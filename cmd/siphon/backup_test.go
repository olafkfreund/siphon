package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/store"
)

// mkState builds a state dir: a db with n audit rows, secrets/x and credentials/c/auth.json.
func mkState(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := s.DB.Begin()
	for i := 0; i < n; i++ {
		store.Audit(tx, time.Now(), "a", "e", 0, "")
	}
	tx.Commit()
	s.Close()
	os.MkdirAll(filepath.Join(dir, "secrets"), 0o700)
	os.WriteFile(filepath.Join(dir, "secrets", "x"), []byte("SECRET"), 0o600)
	os.MkdirAll(filepath.Join(dir, "credentials", "c"), 0o700)
	os.WriteFile(filepath.Join(dir, "credentials", "c", "auth.json"), []byte("LOGIN"), 0o600)
	return dir
}

func create(t *testing.T, dir string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	err := backupCreate(append([]string{"-db", filepath.Join(dir, "state.db")}, args...), &out, &errb, false)
	return errb.String(), err
}

func restore(dir string, in io.Reader, tty bool, args ...string) (string, error) {
	var errb bytes.Buffer
	err := backupRestore(append([]string{"-db", filepath.Join(dir, "state.db")}, args...), in, &errb, tty)
	return errb.String(), err
}

func code(err error) int {
	var ee exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

func auditRows(t *testing.T, dir string) (n int) {
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.DB.QueryRow(`SELECT count(*) FROM audit`).Scan(&n)
	return
}

func TestBackupRoundTrip(t *testing.T) {
	src := mkState(t, 3)
	s, _ := store.Open(filepath.Join(src, "state.db"))
	done := make(chan struct{})
	go func() { // writes while the backup runs
		defer close(done)
		for i := 0; i < 20; i++ {
			s.DB.Exec(`INSERT INTO audit(at, actor, event, job_id, detail) VALUES (1,'a','w',0,'')`)
		}
	}()
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	msg, err := create(t, src, out)
	<-done
	s.Close()
	if err != nil || !strings.Contains(msg, "1 secrets, 1 logins") || strings.Contains(msg, "SECRET") {
		t.Fatalf("%q %v", msg, err)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := create(t, src, out); err == nil {
		t.Fatal("existing target must be refused")
	}
	if _, err := create(t, src, "--force", out); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(out + ".tmp-*"); len(m) != 0 {
		t.Fatal("tmp left", m)
	}

	dst := t.TempDir()
	if _, err := restore(dst, nil, false, out); err != nil {
		t.Fatal(err)
	}
	if auditRows(t, dst) < 3 {
		t.Fatal("db not restored")
	}
	for p, want := range map[string]string{"secrets/x": "SECRET", "credentials/c/auth.json": "LOGIN"} {
		b, _ := os.ReadFile(filepath.Join(dst, p))
		fi, _ := os.Stat(filepath.Join(dst, p))
		if string(b) != want || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %q %v", p, b, fi.Mode())
		}
	}
	for _, p := range []string{"secrets", "credentials", "credentials/c"} {
		if fi, _ := os.Stat(filepath.Join(dst, p)); fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", p, fi.Mode())
		}
	}

	// existing state: no TTY and no --yes exits 2; with --yes the old files move to pre-restore-*
	os.WriteFile(filepath.Join(dst, "secrets", "x"), []byte("NEWER"), 0o600)
	if _, err := restore(dst, nil, false, out); code(err) != 2 {
		t.Fatalf("want 2, got %v", err)
	}
	if _, err := restore(dst, nil, false, "--yes", out); err != nil {
		t.Fatal(err)
	}
	pre, _ := filepath.Glob(filepath.Join(dst, "pre-restore-*"))
	if len(pre) != 1 {
		t.Fatal(pre)
	}
	if b, _ := os.ReadFile(filepath.Join(pre[0], "secrets", "x")); string(b) != "NEWER" {
		t.Fatalf("pre-restore holds %q", b)
	}
	if _, err := os.Stat(filepath.Join(pre[0], "state.db")); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRunning(t *testing.T) {
	dir := mkState(t, 1)
	unlock, err := store.Lock(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := restore(dir, nil, false, "--yes", "nonexistent"); code(err) != 5 {
		t.Fatalf("want 5, got %v", err)
	}
}

func TestRefuseTTY(t *testing.T) {
	if refuseTTY(true) == nil || refuseTTY(false) != nil {
		t.Fatal("refuseTTY")
	}
	var out bytes.Buffer
	err := backupCreate([]string{"-db", filepath.Join(t.TempDir(), "state.db"), "-"}, &out, io.Discard, true)
	if code(err) != 2 {
		t.Fatalf("got %v", err)
	}
}

type ent struct {
	name, link string
	typ        byte
	body       string
}

func archive(ents []ent) *bytes.Reader {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		tw.WriteHeader(&tar.Header{Name: e.name, Linkname: e.link, Typeflag: typ, Mode: 0o600, Size: int64(len(e.body))})
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	return bytes.NewReader(b.Bytes())
}

func TestRestoreRejectsBadArchives(t *testing.T) {
	good, _ := os.ReadFile(filepath.Join(mkState(t, 1), "state.db"))
	man := func(schema string) ent {
		return ent{name: manifestName, body: `{"format":1,"version":"x","schema":"` + schema + `"}`}
	}
	db := ent{name: "state.db", body: string(good)}
	// altered is the good db after one statement: what a hand-made archive could carry
	altered := func(stmt string) ent {
		dir := mkState(t, 1)
		st, err := store.Open(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(stmt); err != nil {
			t.Fatal(err)
		}
		st.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		st.Close()
		b, _ := os.ReadFile(filepath.Join(dir, "state.db"))
		return ent{name: "state.db", body: string(b)}
	}
	cases := map[string][]ent{
		"dotdot":    {man("0001_init.sql"), db, {name: "secrets/../x", body: "x"}},
		"abs":       {man("0001_init.sql"), db, {name: "/abs", body: "x"}},
		"symlink":   {man("0001_init.sql"), db, {name: "secrets/l", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hardlink":  {man("0001_init.sql"), db, {name: "secrets/l", typ: tar.TypeLink, link: "state.db"}},
		"other":     {man("0001_init.sql"), db, {name: "other/file", body: "x"}},
		"dup db":    {man("0001_init.sql"), db, db},
		"newer":     {man("9999_x.sql"), db},
		"corrupt":   {man("0001_init.sql"), {name: "state.db", body: strings.Repeat("not a database ", 500)}},
		"no db":     {man("0001_init.sql")},
		"no manif.": {db},
		// the manifest understates the db's schema, or names none: the db's own schema decides
		"db newer":  {man("0001_init.sql"), altered(`INSERT INTO schema_migrations VALUES ('9999_x.sql')`)},
		"no schema": {man(""), altered(`DELETE FROM schema_migrations`)},
		"trigger":   {man("0001_init.sql"), altered(`CREATE TRIGGER t AFTER INSERT ON audit BEGIN DELETE FROM audit; END`)},
		"view":      {man("0001_init.sql"), altered(`CREATE VIEW v AS SELECT 1`)},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			dir := mkState(t, 2)
			before := snapshot(t, dir)
			_, err := restore(dir, archive(ents), false, "--yes", "-")
			if err == nil {
				t.Fatal("accepted")
			}
			t.Log(err)
			if after := snapshot(t, dir); after != before {
				t.Fatalf("dir changed:\n%s\n--\n%s", before, after)
			}
		})
	}
}

// snapshot lists every path in dir (the lock file aside).
func snapshot(t *testing.T, dir string) string {
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !strings.HasSuffix(p, ".lock") {
			b.WriteString(p + "\n")
		}
		return nil
	})
	return b.String()
}
