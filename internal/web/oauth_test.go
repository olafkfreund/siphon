package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/mcpoauth"
)

// oauthEnv is a web server with one auth.oauth source "o" whose MCP URL points
// at a stub that demands a login and an authorization server that registers a
// client but refuses every code.
func oauthEnv(t *testing.T) *env {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	j := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/prm"`)
		http.Error(w, "login", http.StatusUnauthorized)
	})
	mux.HandleFunc("/prm", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token",
			"registration_endpoint": srv.URL + "/register", "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		j(w, 201, map[string]any{"client_id": "c1", "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		j(w, 400, map[string]string{"error": "invalid_grant", "error_description": "refused"})
	})
	cfg := &config.Config{Sources: map[string]*config.Source{
		"o":     {Type: "mcp", URL: srv.URL + "/mcp", AllowPrivate: true, Auth: &config.Auth{OAuth: &config.OAuth{}}},
		"plain": {Type: "mcp", URL: "https://x.example/mcp"},
		"down":  {Type: "mcp", URL: "http://127.0.0.1:1/mcp", AllowPrivate: true, Auth: &config.Auth{OAuth: &config.OAuth{}}},
	}}
	cfg.Server.PublicURL = "http://127.0.0.1:8080"
	return newEnv(t, func(o *Options) {
		o.Cfg = cfg
		o.OAuth = mcpoauth.New(t.TempDir(), func() *config.Config { return cfg }, time.Now)
	})
}

func TestOAuthAPI(t *testing.T) {
	e := oauthEnv(t)
	for _, c := range [][2]string{{"POST", "/api/sources/o/oauth/login"}, {"GET", "/api/sources/o/oauth"}, {"DELETE", "/api/sources/o/oauth"}} {
		if w := e.do(c[0], c[1], nil, nil); w.Code != 401 {
			t.Errorf("%s %s without token: %d", c[0], c[1], w.Code)
		}
	}
	// A login that can't start says why (a 502, not a bare 500).
	if w := e.do("POST", "/api/sources/down/oauth/login", nil, bearer); w.Code != 502 || !strings.Contains(w.Body.String(), "did not start") {
		t.Errorf("unreachable server: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "/api/sources/plain/oauth", nil, bearer); w.Code != 404 {
		t.Errorf("non-oauth source: %d", w.Code)
	}
	if w := e.do("GET", "/api/sources/o/oauth", nil, bearer); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"none"`) {
		t.Errorf("status: %d %s", w.Code, w.Body.String())
	}
	w := e.do("POST", "/api/sources/o/oauth/login", nil, bearer)
	var out struct{ URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !strings.Contains(out.URL, "/authorize?") {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "/api/sources/o/oauth", nil, bearer); !strings.Contains(w.Body.String(), `"status":"pending"`) {
		t.Errorf("want pending: %s", w.Body.String())
	}
	if w := e.do("GET", "/api/audit", nil, bearer); !strings.Contains(w.Body.String(), "oauth_login_started") || strings.Contains(w.Body.String(), "state=") {
		t.Errorf("audit: %s", w.Body.String())
	}
	if w := e.do("DELETE", "/api/sources/o/oauth", nil, bearer); w.Code != 200 {
		t.Errorf("logout: %d", w.Code)
	}
}

func TestOAuthCallback(t *testing.T) {
	e := oauthEnv(t)
	w := e.do("POST", "/api/sources/o/oauth/login", nil, bearer)
	var out struct{ URL string }
	json.Unmarshal(w.Body.Bytes(), &out)
	u, _ := url.Parse(out.URL)
	state := u.Query().Get("state")

	// public, no token; the page never echoes the code or the state
	w = e.do("GET", "/oauth/callback?state="+state+"&code=SECRETCODE", nil, nil)
	if w.Code != 400 || strings.Contains(w.Body.String(), "SECRETCODE") || strings.Contains(w.Body.String(), state) || !strings.Contains(w.Body.String(), "Login failed") {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	// single-use: the state is gone, and unknown states count toward the limiter
	for i := range 5 {
		w = e.do("GET", "/oauth/callback?state="+state+"&code=x", nil, nil)
		want := 400
		if i == 4 { // 1 failure above + 4 here = 5: the next is blocked before any lookup
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := e.do("GET", "/api/rules", nil, bearer); w.Code != 429 {
		t.Fatalf("same IP should be blocked: %d", w.Code)
	}
}

func TestOAuthPortal(t *testing.T) {
	e := oauthEnv(t)
	c, csrf := e.login()
	withCookie := func(r *http.Request) { r.AddCookie(c) }
	page := e.do("GET", "/sources", nil, withCookie).Body.String()
	if !strings.Contains(page, "/sources/o/oauth") || !strings.Contains(page, "Log in") || strings.Contains(page, "/sources/plain/oauth") {
		t.Fatalf("sources page: %s", page)
	}
	if w := e.do("POST", "/sources/o/oauth", url.Values{}, withCookie); w.Code != 403 {
		t.Fatalf("no csrf: %d", w.Code)
	}
	if w := e.do("POST", "/sources/o/oauth", url.Values{"csrf": {csrf}}, nil); w.Code != 401 {
		t.Fatalf("no session: %d", w.Code)
	}
	w := e.do("POST", "/sources/o/oauth", url.Values{"csrf": {csrf}}, withCookie)
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "/authorize?") {
		t.Fatalf("login: %d %v", w.Code, w.Header())
	}
	if w := e.do("POST", "/sources/plain/oauth", url.Values{"csrf": {csrf}}, withCookie); w.Code != 404 {
		t.Fatalf("plain source: %d", w.Code)
	}
}
