package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Plan #23 step 5: the Services page creates ordinary items; tokens and the
// webhook secret are write-only (the secret is shown exactly once); Test
// reports the user without echoing the token.
func TestServicesGitHubAndGitLab(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/user" && r.Header.Get("PRIVATE-TOKEN") == "glpat-SECRET2" {
			w.Write([]byte(`{"username":"olaf"}`))
			return
		}
		http.Error(w, "nope glpat-SECRET2", 401)
	}))
	defer gl.Close()
	u, _ := url.Parse(gl.URL)
	ce := newCfgEnvFile(t, strings.Replace(cfgFile, "server: { sandbox: none, db: DIR/s.db }",
		`server: { sandbox: none, db: DIR/s.db, public_url: "https://siphon.example", services: { private_endpoints: ["`+u.Host+`"] } }`, 1))

	// GitHub, remote MCP + webhook
	w := ce.post("/services/github", url.Values{"name": {"ghub"}, "token": {"ghp_SECRET1"}, "mode": {"remote"}, "webhook": {"on"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://siphon.example/hook/ghub-hooks") {
		t.Fatalf("github: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("result page must not be cached")
	}
	cfg := ce.cur.Load()
	src, hook := cfg.Sources["ghub"], cfg.Sources["ghub-hooks"]
	if src == nil || src.URL != githubMCP || src.Auth == nil || src.Auth.Bearer.Value != "ghp_SECRET1" || hook == nil || hook.Signature != "github" {
		t.Fatal("github items not applied")
	}
	if fi, err := os.Stat(strings.TrimPrefix(src.Auth.Bearer.Ref, "file:")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v", err)
	}
	secret := hook.Secret.Value
	if len(secret) != 64 || !strings.Contains(w.Body.String(), secret) {
		t.Fatal("webhook secret not shown on the result page")
	}
	for _, p := range []string{"/services", "/config/sources/ghub", "/config/sources/ghub-hooks", "/history", "/history/export"} {
		b := ce.get(p).Body.String()
		if strings.Contains(b, "ghp_SECRET1") || strings.Contains(b, secret) {
			t.Fatalf("secret visible again on %s", p)
		}
	}

	// local mode needs the package
	if w := ce.post("/services/github", url.Values{"name": {"gh2"}, "token": {"t"}, "mode": {"local"}}); w.Code != 422 || !strings.Contains(w.Body.String(), "mcp_packages") {
		t.Fatalf("local without package: %d", w.Code)
	}

	// GitLab, self-hosted (listed), REST + token webhook
	w = ce.post("/services/gitlab", url.Values{"name": {"gl"}, "base": {gl.URL}, "project": {"grp/proj"}, "token": {"glpat-SECRET2"}, "webhook": {"on"}})
	if w.Code != 200 {
		t.Fatalf("gitlab: %d %s", w.Code, w.Body.String())
	}
	cfg = ce.cur.Load()
	if s := cfg.Sources["gl"]; s == nil || !s.AllowPrivate || !strings.Contains(s.URL, "/api/v4/projects/grp%2Fproj/merge_requests") {
		t.Fatalf("gitlab source: %+v", s)
	}
	if h := cfg.Sources["gl-hooks"]; h == nil || h.Signature != "token" || h.TokenHeader != "X-Gitlab-Token" {
		t.Fatal("gitlab webhook")
	}
	test := ce.post("/services/gl/test", nil).Body.String()
	if !strings.Contains(test, "olaf") || strings.Contains(test, "glpat-SECRET2") {
		t.Fatalf("test: %s", test)
	}
}

// F6/F7: an existing name (or its -hooks twin) is never replaced, GitLab needs
// https unless the host is local or listed, and a failed apply is shown.
func TestServicesNamesHTTPSAndApplyError(t *testing.T) {
	ce := newCfgEnv(t)
	form := func(name, base string) url.Values {
		return url.Values{"name": {name}, "base": {base}, "project": {"g/p"}, "token": {"glpat-X"}, "webhook": {"on"}}
	}
	if w := ce.post("/services/gitlab", form("gl", "http://gitlab.lan")); w.Code != 422 || !strings.Contains(w.Body.String(), "must be https") {
		t.Fatalf("http gitlab: %d %s", w.Code, w.Body.String())
	}
	if ce.cur.Load().Sources["gl"] != nil {
		t.Fatal("created over http")
	}
	if w := ce.post("/services/gitlab", form("gl", "http://127.0.0.1:9")); w.Code != 200 {
		t.Fatalf("loopback http: %d", w.Code)
	}
	for _, name := range []string{"gl", "gl-hooks", "gh"} { // a made source, its -hooks twin, a file-defined source
		w := ce.post("/services/github", url.Values{"name": {name}, "token": {"t"}, "mode": {"remote"}})
		if w.Code != 422 || !strings.Contains(w.Body.String(), "already exists; pick another name or edit it") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	ce.applyFail.Store(true)
	w := ce.post("/services/github", url.Values{"name": {"gh9"}, "token": {"t"}, "mode": {"remote"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "applying it live failed") || !strings.Contains(w.Body.String(), "apply boom") {
		t.Fatalf("apply failure not shown: %d %s", w.Code, w.Body.String())
	}
}
