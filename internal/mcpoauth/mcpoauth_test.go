package mcpoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/siphon/internal/config"
)

// clock is an injectable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeAS is a minimal authorization server plus an MCP server behind it.
type fakeAS struct {
	*httptest.Server
	mu         sync.Mutex
	registers  int
	tokens     int
	n          int
	challenge  map[string]string // code -> PKCE challenge
	access     map[string]bool   // valid access tokens
	refresh    string            // the one valid refresh token
	expiresIn  int
	noRefresh  bool
	issuerSent string // iss the /authorize answer carries ("" = the real one)
}

func newAS(t *testing.T) *fakeAS {
	a := &fakeAS{challenge: map[string]string{}, access: map[string]bool{}, expiresIn: 3600}
	mux := http.NewServeMux()
	a.Server = httptest.NewServer(mux)
	t.Cleanup(a.Close)
	j := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"resource": a.URL + "/mcp", "authorization_servers": []string{a.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"issuer": a.URL, "authorization_endpoint": a.URL + "/authorize", "token_endpoint": a.URL + "/token",
			"registration_endpoint": a.URL + "/register", "authorization_response_iss_parameter_supported": true,
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},
			"scopes_supported": []string{"read", "offline_access"}})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.registers++
		id := fmt.Sprintf("dcr-%d", a.registers)
		a.mu.Unlock()
		w.WriteHeader(201)
		j(w, map[string]any{"client_id": id, "client_secret": "dcr-secret", "token_endpoint_auth_method": "client_secret_post"})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("resource") != a.URL+"/mcp" || q.Get("response_type") != "code" {
			http.Error(w, "bad request", 400)
			return
		}
		a.mu.Lock()
		a.n++
		code := fmt.Sprintf("code-%d", a.n)
		a.challenge[code] = q.Get("code_challenge")
		iss := a.issuerSent
		a.mu.Unlock()
		if iss == "" {
			iss = a.URL
		}
		j(w, map[string]string{"code": code, "state": q.Get("state"), "iss": iss, "scope": q.Get("scope"), "client_id": q.Get("client_id")})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		a.mu.Lock()
		defer a.mu.Unlock()
		a.tokens++
		bad := func() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "nope"})
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			ch, ok := a.challenge[r.PostForm.Get("code")]
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !ok || ch != base64.RawURLEncoding.EncodeToString(sum[:]) {
				bad()
				return
			}
			delete(a.challenge, r.PostForm.Get("code"))
		case "refresh_token":
			if r.PostForm.Get("refresh_token") != a.refresh {
				bad()
				return
			}
		}
		a.n++
		at := fmt.Sprintf("at-%d", a.n)
		a.access[at] = true
		out := map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": a.expiresIn}
		if !a.noRefresh {
			a.refresh = fmt.Sprintf("rt-%d", a.n)
			out["refresh_token"] = a.refresh
		}
		j(w, out)
	})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	verify := func(_ context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.access[tok] {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Expiration: time.Now().Add(time.Hour)}, nil
	}
	mux.Handle("/mcp", auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{ResourceMetadataURL: a.URL + "/.well-known/oauth-protected-resource"})(h))
	return a
}

type env struct {
	t   *testing.T
	as  *fakeAS
	clk *clock
	cfg *config.Config
	dir string
	m   *Manager
}

func newEnv(t *testing.T, oc config.OAuth) *env {
	as := newAS(t)
	e := &env{t: t, as: as, clk: &clock{t: time.Now()}, dir: filepath.Join(t.TempDir(), ".mcp")}
	e.cfg = &config.Config{Sources: map[string]*config.Source{"s": {Type: "mcp", URL: as.URL + "/mcp", AllowPrivate: true, Auth: &config.Auth{OAuth: &oc}}}}
	e.cfg.Server.PublicURL = "http://127.0.0.1:8080"
	e.m = e.newManager()
	return e
}

func (e *env) newManager() *Manager {
	return New(e.dir, func() *config.Config { return e.cfg }, e.clk.Now)
}

// login runs a whole login: Start, "the browser" fetching the authorization URL, Complete.
func (e *env) login(m *Manager) error {
	e.t.Helper()
	u, err := m.Start(context.Background(), "s")
	if err != nil {
		return err
	}
	resp, err := http.Get(u)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b struct{ Code, State, Iss string }
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &b); err != nil {
		e.t.Fatalf("authorize: %s", body)
	}
	_, err = m.Complete(b.State, b.Code, b.Iss, "")
	return err
}

func (e *env) login1(m *Manager) {
	e.t.Helper()
	if err := e.login(m); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) file() string { return filepath.Join(e.dir, "s", file) }

func noSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(err.Error(), s) {
			t.Errorf("error leaks %q: %v", s, err)
		}
	}
}

func TestLoginDynamicAndPoll(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	if s, _ := e.m.Status("s"); s != "none" {
		t.Fatalf("status %s", s)
	}
	if _, err := e.m.Token(context.Background(), "s"); !errors.Is(err, ErrLoginRequired) || !strings.Contains(err.Error(), "siphon connect oauth s") {
		t.Fatalf("want login required with hint: %v", err)
	}
	e.login1(e.m)
	if s, exp := e.m.Status("s"); s != "ok" || exp.IsZero() {
		t.Fatalf("status %s %v", s, exp)
	}
	if e.as.registers != 1 {
		t.Fatalf("registers %d", e.as.registers)
	}
	// the steady-state handler lets a real MCP connection through
	tr := &mcp.StreamableClientTransport{Endpoint: e.cfg.Sources["s"].URL, HTTPClient: http.DefaultClient, MaxRetries: -1, DisableStandaloneSSE: true, OAuthHandler: e.m.Handler("s")}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs.Close()
	// file is 0600 in a 0700 dir
	fi, err := os.Stat(e.file())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	if di, _ := os.Stat(filepath.Dir(e.file())); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}
	var st stored
	b, _ := os.ReadFile(e.file())
	json.Unmarshal(b, &st)
	if !st.Dynamic || st.ClientSecret != "dcr-secret" || st.Resource != e.cfg.Sources["s"].URL || st.Issuer != e.as.URL {
		t.Fatalf("stored: %+v", st)
	}
}

func TestLoginPreregistered(t *testing.T) {
	oc := config.OAuth{ClientID: "pre", Scopes: []string{"read"}}
	oc.ClientSecret.Value = "pre-secret"
	e := newEnv(t, oc)
	e.login1(e.m)
	if e.as.registers != 0 {
		t.Fatalf("registered %d times", e.as.registers)
	}
	b, _ := os.ReadFile(e.file())
	var st stored
	json.Unmarshal(b, &st)
	if st.ClientID != "pre" || st.ClientSecret != "" || st.Dynamic || strings.Contains(string(b), "pre-secret") {
		t.Fatalf("stored: %s", b)
	}
	if !slices.Contains(st.Scopes, "read") || slices.Contains(st.Scopes, "write") { // the SDK adds offline_access itself
		t.Fatalf("scopes %v", st.Scopes)
	}
	// a changed client_id invalidates the login
	e.cfg.Sources["s"].Auth.OAuth.ClientID = "other"
	if s, _ := e.m.Status("s"); s != "none" {
		t.Fatalf("status %s", s)
	}
}

func TestReloginReusesDCRClientAndSurvivesRestart(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.login1(e.m)
	m2 := e.newManager() // a restart
	if s, _ := m2.Status("s"); s != "ok" {
		t.Fatalf("status after restart %s", s)
	}
	if tok, err := m2.Token(context.Background(), "s"); err != nil || tok == "" {
		t.Fatalf("token after restart: %v", err)
	}
	e.login1(m2)
	if e.as.registers != 1 {
		t.Fatalf("re-login registered again: %d", e.as.registers)
	}
}

func TestRefreshWritesBack(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.login1(e.m)
	e.as.expiresIn = 100 * 3600 // the refreshed token outlives the clock jump
	first, _ := e.m.Token(context.Background(), "s")
	before, _ := os.ReadFile(e.file())
	e.clk.Add(2 * time.Hour)
	second, err := e.m.Token(context.Background(), "s")
	if err != nil || second == first {
		t.Fatalf("refresh: %q %v", second, err)
	}
	after, _ := os.ReadFile(e.file())
	if string(after) == string(before) || !strings.Contains(string(after), second) {
		t.Fatal("refreshed token not written back")
	}
	if _, err := os.Stat(e.file() + ".prev"); err != nil {
		t.Fatalf("no .prev: %v", err)
	}
	// a new Manager reads the written-back token
	if got, _ := e.newManager().Token(context.Background(), "s"); got != second {
		t.Fatalf("restart got %q want %q", got, second)
	}
}

func TestInvalidGrantMeansLoginRequired(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.login1(e.m)
	rt := e.as.refresh
	e.as.mu.Lock()
	e.as.refresh = "revoked"
	e.as.mu.Unlock()
	e.clk.Add(2 * time.Hour)
	_, err := e.m.Token(context.Background(), "s")
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("want ErrLoginRequired: %v", err)
	}
	noSecrets(t, err, rt)
	// the rejected refresh token is never retried: even a server that would
	// now accept it gets no second request
	e.as.mu.Lock()
	e.as.refresh = rt
	e.as.mu.Unlock()
	if _, err := e.m.Token(context.Background(), "s"); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("retried a rejected refresh token: %v", err)
	}
	if s, _ := e.m.Status("s"); s != "expired" {
		t.Fatalf("status %s", s)
	}
	// logging in again recovers
	e.login1(e.m)
	if s, _ := e.m.Status("s"); s != "ok" {
		t.Fatalf("status %s", s)
	}
}

func TestChangedURLInvalidatesLogin(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.login1(e.m)
	e.cfg.Sources["s"].URL += "2"
	if _, err := e.m.Token(context.Background(), "s"); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("want login required: %v", err)
	}
}

func TestStateSingleUseAndExpires(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	u, err := e.m.Start(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, u)
	// expired: advance the injected clock past the 10 minutes
	e.clk.Add(11 * time.Minute)
	if _, err := e.m.Complete(state, "x", "", ""); err == nil {
		t.Fatal("expired state accepted")
	}
	// single-use: a fresh login's state works once
	e.clk.Add(-11 * time.Minute)
	u, err = e.m.Start(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	state = mustState(t, u)
	if _, err := e.m.Complete(state, "bad-code", "", ""); err == nil {
		t.Fatal("bad code accepted")
	}
	if _, err := e.m.Complete(state, "bad-code", "", ""); err == nil {
		t.Fatal("state used twice")
	}
	if _, err := e.m.Complete("bogus", "x", "", ""); err == nil {
		t.Fatal("unknown state accepted")
	}
}

func mustState(t *testing.T, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("bad url %q", raw)
	}
	return u.Query().Get("state")
}

func TestIssMismatchFails(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.as.issuerSent = "https://evil.example"
	if err := e.login(e.m); err == nil {
		t.Fatal("iss mismatch accepted")
	}
	if s, _ := e.m.Status("s"); s != "none" {
		t.Fatalf("status %s", s)
	}
}

func TestAuthorizationRefused(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	u, err := e.m.Start(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.m.Complete(mustState(t, u), "", "", "access_denied")
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err %v", err)
	}
}

func TestNewStartCancelsEarlierLogin(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	u1, _ := e.m.Start(context.Background(), "s")
	e.m.Start(context.Background(), "s")
	deadline := time.After(5 * time.Second)
	for {
		if _, err := e.m.Complete(mustState(t, u1), "x", "", ""); err != nil {
			break // the first login's state is gone
		}
		select {
		case <-deadline:
			t.Fatal("first login not cancelled")
		default:
		}
	}
}

func TestLogoutAndStartErrors(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.login1(e.m)
	if err := e.m.Logout("s"); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.m.Status("s"); s != "none" {
		t.Fatalf("status %s", s)
	}
	if _, err := e.m.Start(context.Background(), "nope"); err == nil {
		t.Fatal("unknown source")
	}
	e.cfg.Server.PublicURL = ""
	if _, err := e.m.Start(context.Background(), "s"); err == nil {
		t.Fatal("no public_url")
	}
}

func TestStartTimesOut(t *testing.T) {
	// an MCP server that never answers: Start must give up
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)
	old := startWait
	startWait = 100 * time.Millisecond
	defer func() { startWait = old }()
	cfg := &config.Config{Sources: map[string]*config.Source{"s": {Type: "mcp", URL: srv.URL, AllowPrivate: true, Auth: &config.Auth{OAuth: &config.OAuth{}}}}}
	cfg.Server.PublicURL = "http://127.0.0.1:1"
	m := New(t.TempDir(), func() *config.Config { return cfg }, time.Now)
	if _, err := m.Start(context.Background(), "s"); err == nil {
		t.Fatal("want timeout")
	}
}

func TestNoTokenInErrors(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.login1(e.m)
	b, _ := os.ReadFile(e.file())
	var st stored
	json.Unmarshal(b, &st)
	secrets := []string{st.Token.Access, st.Token.Refresh, st.ClientSecret}
	// a failing refresh whose raw body carries the token
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.WriteHeader(400)
		fmt.Fprintf(w, "boom refresh_token=%s", r.PostForm.Get("refresh_token"))
	}))
	defer bad.Close()
	st.TokenURL = bad.URL
	nb, _ := json.Marshal(st)
	os.WriteFile(e.file(), nb, 0o600)
	e.clk.Add(2 * time.Hour)
	_, err := e.m.Token(context.Background(), "s")
	if err == nil {
		t.Fatal("want error")
	}
	noSecrets(t, err, secrets...)
}

func TestClipRedactsTokenLikeText(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlLXZhbHVl"
	got := clip("dial tcp: refused, token "+jwt+" and opaque_0123456789abcdefghijklmn", 300)
	if strings.Contains(got, "eyJ") || strings.Contains(got, "0123456789abcdefghijklmn") || !strings.Contains(got, "dial tcp: refused") {
		t.Fatalf("clip: %q", got)
	}
}

func (e *env) pin(v string) { e.cfg.Sources["s"].Auth.OAuth.Issuer = v }

func TestIssuerPinPreregistered(t *testing.T) {
	oc := config.OAuth{ClientID: "pre"}
	oc.ClientSecret.Value = "pre-secret"
	e := newEnv(t, oc)
	b := newAS(t)
	e.pin(b.URL)
	u, err := e.m.Start(context.Background(), "s")
	if u != "" || err == nil || !strings.Contains(err.Error(), b.URL) || !strings.Contains(err.Error(), e.as.URL) {
		t.Fatalf("want no authorization URL and an error naming both issuers: %q %v", u, err)
	}
	noSecrets(t, err, "pre-secret")
	if e.as.tokens != 0 {
		t.Fatalf("token endpoint of the other server hit %d times", e.as.tokens)
	}
	e.pin(e.as.URL)
	e.login1(e.m)
	raw, _ := os.ReadFile(e.file())
	var st stored
	json.Unmarshal(raw, &st)
	if st.Issuer != e.as.URL {
		t.Fatalf("stored issuer %q", st.Issuer)
	}
}

func TestIssuerPinDynamic(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	b := newAS(t)
	e.pin(b.URL)
	u, err := e.m.Start(context.Background(), "s")
	if u != "" || err == nil || !strings.Contains(err.Error(), b.URL) || !strings.Contains(err.Error(), e.as.URL) {
		t.Fatalf("want no authorization URL and an error naming both issuers: %q %v", u, err)
	}
	if e.as.registers != 0 || e.as.tokens != 0 || b.registers != 1 {
		t.Fatalf("as registers=%d tokens=%d, b registers=%d", e.as.registers, e.as.tokens, b.registers)
	}
	e.pin(e.as.URL)
	e.login1(e.m)
	raw, _ := os.ReadFile(e.file())
	var st stored
	json.Unmarshal(raw, &st)
	if e.as.registers != 1 || !st.Dynamic {
		t.Fatalf("registers %d, stored %s", e.as.registers, raw)
	}
	e.login1(e.m)
	if e.as.registers != 1 {
		t.Fatalf("re-login registered again: %d", e.as.registers)
	}
}

func TestIssuerPinRefusesOldLogin(t *testing.T) {
	e := newEnv(t, config.OAuth{})
	e.login1(e.m)
	e.pin(e.as.URL + "/") // a trailing slash is ignored
	if s, _ := e.m.Status("s"); s != "ok" {
		t.Fatalf("status %s", s)
	}
	e.pin(newAS(t).URL)
	if s, _ := e.m.Status("s"); s != "none" {
		t.Fatalf("status %s", s)
	}
	raw, _ := os.ReadFile(e.file())
	var st stored
	json.Unmarshal(raw, &st)
	st.Issuer = ""
	raw, _ = json.Marshal(st)
	os.WriteFile(e.file(), raw, 0o600)
	e.pin(e.as.URL)
	if s, _ := e.m.Status("s"); s != "none" {
		t.Fatalf("empty stored issuer accepted: %s", s)
	}
}
