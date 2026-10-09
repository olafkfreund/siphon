package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectOAuth(t *testing.T) {
	polls, status := 0, "pending"
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		switch {
		case r.URL.Path == "/api/sources/nope/oauth/login":
			http.Error(w, `{"error":"no such OAuth source"}`, 404)
		case r.Method == "POST":
			w.Write([]byte(`{"url":"https://as.example/authorize?x=1"}`))
		case r.Method == "DELETE":
			w.Write([]byte(`{"status":"none"}`))
		default:
			polls++
			if polls >= 3 && status == "pending" {
				status = "ok"
			}
			w.Write([]byte(`{"status":"` + status + `"}`))
		}
	}))
	defer api.Close()
	dir := t.TempDir()
	tokf := filepath.Join(dir, "token")
	os.WriteFile(tokf, []byte("tok\n"), 0o600)
	t.Setenv("SIPHON_URL", api.URL)
	t.Setenv("SIPHON_TOKEN_FILE", tokf)
	sleeps := 0
	oldSleep, oldPolls := oauthSleep, oauthPolls
	oauthSleep, oauthPolls = func() { sleeps++ }, 5
	defer func() { oauthSleep, oauthPolls = oldSleep, oldPolls }()
	run := func(args ...string) (int, string, string) {
		var o, er bytes.Buffer
		c := &cli{in: strings.NewReader(""), out: &o, errw: &er, g: globals{output: "text"}}
		code := c.run(args)
		return code, o.String(), er.String()
	}

	code, out, er := run("connect", "oauth", "s")
	if code != 0 || !strings.Contains(out, "https://as.example/authorize?x=1") || !strings.Contains(out, "logged in") || sleeps != 2 {
		t.Fatalf("login: %d sleeps=%d\n%s\n%s", code, sleeps, out, er)
	}
	// -o json: stdout is the result, and the URL still reaches the person on stderr
	polls, status = 0, "pending"
	if code, out, er = run("connect", "oauth", "s", "-o", "json"); code != 0 || !strings.Contains(er, "authorize?x=1") || !strings.Contains(out, `"status": "ok"`) {
		t.Fatalf("json: %d\n%s\n%s", code, out, er)
	}
	if code, out, _ = run("connect", "oauth", "s", "--no-wait"); code != 0 || !strings.Contains(out, "authorize") || strings.Contains(out, "logged in") {
		t.Fatalf("no-wait: %d %s", code, out)
	}
	if code, out, _ = run("connect", "oauth", "s", "--logout"); code != 0 || !strings.Contains(out, "logged out") {
		t.Fatalf("logout: %d %s", code, out)
	}
	if code, _, er = run("connect", "oauth", "nope"); code == 0 || !strings.Contains(er, "no such OAuth source") {
		t.Fatalf("unknown: %d %s", code, er)
	}
	if code, _, _ = run("connect", "oauth"); code != 2 {
		t.Fatalf("usage: %d", code)
	}
	// the login ended without a token: status falls back to none
	polls, status = 0, "none"
	if code, _, er = run("connect", "oauth", "s"); code == 0 || !strings.Contains(er, "did not complete") {
		t.Fatalf("failed login: %d %s", code, er)
	}
	// it gives up after the poll budget
	polls, status = -100, "pending"
	if code, _, er = run("connect", "oauth", "s"); code == 0 || !strings.Contains(er, "timed out") {
		t.Fatalf("timeout: %d %s", code, er)
	}
}
