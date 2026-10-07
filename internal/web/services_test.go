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
	w := ce.post("/services/github", url.Values{"name": {"gh"}, "token": {"ghp_SECRET1"}, "mode": {"remote"}, "webhook": {"on"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://siphon.example/hook/gh-hooks") {
		t.Fatalf("github: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("result page must not be cached")
	}
	cfg := ce.cur.Load()
	src, hook := cfg.Sources["gh"], cfg.Sources["gh-hooks"]
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
	for _, p := range []string{"/services", "/config/sources/gh", "/config/sources/gh-hooks", "/history", "/history/export"} {
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
