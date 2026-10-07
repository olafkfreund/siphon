package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolate(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, k := range []string{"SIPHON_URL", "SIPHON_TOKEN", "SIPHON_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	return dir
}

func TestResolvePrecedence(t *testing.T) {
	isolate(t)
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.(*Error).Hint, "siphon login") {
		t.Fatalf("not logged in: %v", err)
	}
	if err := Save(Conn{"http://file:1", "tok-file"}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(Path())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("client.yaml mode %v", fi.Mode())
	}
	c, err := Resolve("", "")
	if err != nil || c.URL != "http://file:1" || c.Token != "tok-file" {
		t.Fatalf("file: %+v %v", c, err)
	}
	t.Setenv("SIPHON_URL", "http://env:2/")
	t.Setenv("SIPHON_TOKEN", "tok-env")
	if c, _ = Resolve("", ""); c.URL != "http://env:2" || c.Token != "tok-env" {
		t.Fatalf("env: %+v", c)
	}
	tf := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tf, []byte("tok-envfile\n"), 0o600)
	t.Setenv("SIPHON_TOKEN_FILE", tf)
	if c, _ = Resolve("", ""); c.Token != "tok-envfile" {
		t.Fatalf("env file beats env token: %+v", c)
	}
	tf2 := filepath.Join(t.TempDir(), "tok2")
	os.WriteFile(tf2, []byte("tok-flag"), 0o600)
	if c, _ = Resolve("http://flag:3", tf2); c.URL != "http://flag:3" || c.Token != "tok-flag" {
		t.Fatalf("flags: %+v", c)
	}
}

func TestLooseClientFileRefused(t *testing.T) {
	isolate(t)
	Save(Conn{"http://x:1", "tok-secret"})
	os.Chmod(Path(), 0o640)
	_, err := Resolve("", "")
	e, ok := err.(*Error)
	if !ok || !strings.Contains(e.Hint, "chmod 600") || strings.Contains(e.Error()+e.Hint, "tok-secret") {
		t.Fatalf("%v", err)
	}
	if err := Remove(); err != nil {
		t.Fatal(err)
	}
	if err := Remove(); err != nil {
		t.Fatalf("remove twice: %v", err)
	}
}

func TestCleanURLRefusesCredentials(t *testing.T) {
	for _, bad := range []string{"ftp://x", "http://u:p@x:1", "http://x:1?token=abc", "x:1", ""} {
		if _, err := CleanURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDoErrorsAndHeaders(t *testing.T) {
	var gotAuth, gotActor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotActor = r.Header.Get("Authorization"), r.Header.Get("X-Siphon-Actor")
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte(`{"a":1}`))
		case "/invalid":
			w.WriteHeader(422)
			w.Write([]byte(`{"error":"x\ny","errors":["x","y"],"warnings":[]}`))
		case "/missing":
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no such item"}`))
		case "/stale":
			w.WriteHeader(409)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	t.Setenv("USER", "olaf")
	c := New(Conn{srv.URL, "tok-SECRET"})
	var out map[string]int
	if err := c.Do("GET", "/ok", nil, &out); err != nil || out["a"] != 1 || gotAuth != "Bearer tok-SECRET" || gotActor != "cli:olaf" {
		t.Fatalf("%v %v %q %q", err, out, gotAuth, gotActor)
	}
	for path, code := range map[string]int{"/invalid": 3, "/missing": 4, "/stale": 5, "/boom": 1} {
		err := c.Do("GET", path, nil, nil)
		e, ok := err.(*Error)
		if !ok || e.ExitCode() != code || e.Hint == "" || len(e.Errors) == 0 && path == "/invalid" {
			t.Errorf("%s: %+v", path, err)
		}
		b, _ := json.Marshal(e)
		if !strings.Contains(string(b), `"error"`) || !strings.Contains(string(b), `"errors"`) || !strings.Contains(string(b), `"hint"`) || strings.Contains(string(b), "SECRET") {
			t.Errorf("json %s", b)
		}
	}
	srv.Close()
	err := c.Do("GET", "/ok", nil, nil)
	if e, ok := err.(*Error); !ok || strings.Contains(e.Error()+e.Hint, "tok-SECRET") || e.Hint == "" {
		t.Fatalf("unreachable: %v", err)
	}
	t.Setenv("USER", "a b/c")
	if a := Actor(); a != "cli:a_b_c" {
		t.Fatalf("actor %q", a)
	}
}
