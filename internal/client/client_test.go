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

func TestResolvePairing(t *testing.T) {
	isolate(t)
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.(*Error).Hint, "siphon login") {
		t.Fatalf("not logged in: %v", err)
	}
	// no saved login: SIPHON_URL goes with the env token
	t.Setenv("SIPHON_URL", "http://127.0.0.1:2/")
	t.Setenv("SIPHON_TOKEN", "tok-env")
	if c, err := Resolve("", ""); err != nil || c.URL != "http://127.0.0.1:2" || c.Token != "tok-env" {
		t.Fatalf("env: %+v %v", c, err)
	}
	// -url alone has no token to go with
	t.Setenv("SIPHON_TOKEN", "")
	if _, err := Resolve("http://127.0.0.1:9", ""); err == nil || err.(*Error).ExitCode() != ExitUsage {
		t.Fatalf("-url without a token: %v", err)
	}
	tf := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tf, []byte("tok-flag\n"), 0o600)
	if c, err := Resolve("http://127.0.0.1:3", tf); err != nil || c.URL != "http://127.0.0.1:3" || c.Token != "tok-flag" {
		t.Fatalf("flags: %+v %v", c, err)
	}
	// a saved login is a pair: SIPHON_URL does not replace its URL
	if err := Save(Conn{"http://127.0.0.1:1", "tok-file"}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(Path())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("client.yaml mode %v", fi.Mode())
	}
	if c, err := Resolve("", ""); err != nil || c.URL != "http://127.0.0.1:1" || c.Token != "tok-file" {
		t.Fatalf("saved pair beats SIPHON_URL: %+v %v", c, err)
	}
	// -url equal to the saved URL uses the saved token; another one must bring its own
	if c, err := Resolve("http://127.0.0.1:1/", ""); err != nil || c.Token != "tok-file" {
		t.Fatalf("same url: %+v %v", c, err)
	}
	if c, err := Resolve("http://127.0.0.1:9", ""); err == nil {
		t.Fatalf("saved token sent to another url: %+v", c)
	}
	if c, err := Resolve("http://127.0.0.1:9", tf); err != nil || c.URL != "http://127.0.0.1:9" || c.Token != "tok-flag" {
		t.Fatalf("other url with its token: %+v %v", c, err)
	}
	// an explicit token goes to the saved URL only
	t.Setenv("SIPHON_TOKEN", "tok-env")
	if c, _ := Resolve("", ""); c.URL != "http://127.0.0.1:1" || c.Token != "tok-env" {
		t.Fatalf("env token: %+v", c)
	}
	t.Setenv("SIPHON_TOKEN_FILE", tf)
	if c, _ := Resolve("", ""); c.Token != "tok-flag" {
		t.Fatalf("env file beats env token: %+v", c)
	}
}

func TestHTTPToRemoteRefused(t *testing.T) {
	isolate(t)
	for _, u := range []string{"http://example.com", "http://10.0.0.5:8080", "http://[2001:db8::1]:1"} {
		if _, err := CleanURL(u); err == nil || err.(*Error).Hint == "" {
			t.Errorf("%s accepted", u)
		}
	}
	for _, u := range []string{"https://example.com", "http://localhost:1", "http://127.0.0.2:1", "http://[::1]:1"} {
		if _, err := CleanURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	t.Setenv("SIPHON_INSECURE_HTTP", "1")
	if _, err := CleanURL("http://example.com"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIPHON_INSECURE_HTTP", "")
	InsecureHTTP = true
	defer func() { InsecureHTTP = false }()
	if _, err := CleanURL("http://example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestClientFileOwner(t *testing.T) {
	isolate(t)
	Save(Conn{"http://127.0.0.1:1", "tok"})
	uid = func() int { return os.Getuid() + 1 }
	defer func() { uid = os.Getuid }()
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("%v", err)
	}
}

func TestLooseClientFileRefused(t *testing.T) {
	isolate(t)
	Save(Conn{"http://127.0.0.1:1", "tok-secret"})
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
