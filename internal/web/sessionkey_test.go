package web

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionKeyCreateAndReload(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadSessionKey(dir)
	if err != nil || len(k1) != 32 {
		t.Fatalf("create: %v len=%d", err, len(k1))
	}
	st, _ := os.Stat(filepath.Join(dir, sessionKeyFile))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	k2, err := LoadSessionKey(dir)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("reload differs: %v", err)
	}
}

func TestSessionKeyWrongSizeReplaced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, sessionKeyFile)
	os.WriteFile(p, []byte("short"), 0o600)
	k, err := LoadSessionKey(dir)
	if err != nil || len(k) != 32 {
		t.Fatalf("replace: %v len=%d", err, len(k))
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, k) {
		t.Fatal("file not replaced")
	}
	if m, _ := filepath.Glob(p + ".*"); len(m) != 0 {
		t.Fatalf("temp left: %v", m)
	}
}

func TestSessionKeyReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if k, err := LoadSessionKey(dir); err == nil {
		t.Fatalf("want error, got key len %d", len(k))
	}
	if e, _ := os.ReadDir(dir); len(e) != 0 {
		t.Fatalf("wrote %v", e)
	}
}

func TestSessionKeyLoosePermsTightened(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, sessionKeyFile)
	want := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(p, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil { // past the umask
		t.Fatal(err)
	}
	got, err := LoadSessionKey(dir)
	if err != nil || string(got) != string(want) {
		t.Fatalf("key changed or error: %v", err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode().Perm(), err)
	}
}
