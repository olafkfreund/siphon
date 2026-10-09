package web

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/olafkfreund/siphon/internal/config"
)

const testCode = "the-auth-code-123"

// idp is a minimal OIDC provider: discovery, JWKS and a token endpoint that
// returns whatever ID token the test queued.
type idp struct {
	*httptest.Server
	key, other *rsa.PrivateKey
	claims     map[string]any
	signWith   *rsa.PrivateKey
	lastToken  string
	verifier   string
}

func newIdP(t *testing.T) *idp {
	p := &idp{}
	p.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	p.other, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": p.URL, "authorization_endpoint": p.URL + "/auth", "token_endpoint": p.URL + "/token",
			"jwks_uri": p.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.verifier = r.PostFormValue("code_verifier")
		if r.PostFormValue("code") != testCode {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		k := p.signWith
		if k == nil {
			k = p.key
		}
		sg, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: k}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		p.lastToken, _ = jwt.Signed(sg).Claims(p.claims).Serialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": p.lastToken})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func oidcEnv(t *testing.T, p *idp, mod func(*config.OIDC)) *env {
	return newEnv(t, func(o *Options) {
		oc := &config.OIDC{Issuer: p.URL, ClientID: "cid", AllowPrivate: true, Roles: config.Roles{
			Admin:    config.RoleMatch{Groups: []string{"adm"}},
			Operator: config.RoleMatch{Groups: []string{"ops"}},
			Viewer:   config.RoleMatch{Groups: []string{"all"}, Emails: []string{"V@Example.com"}},
		}}
		if mod != nil {
			mod(oc)
		}
		o.Cfg.Server.OIDC = oc
		o.Cfg.Server.PublicURL = "http://127.0.0.1:8080"
	})
}

func fromIP(ip string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = ip + ":1" }
}

// begin starts a sign-in and queues an ID token built from the nonce.
func (e *env) begin(t *testing.T, p *idp, ip string, claims map[string]any) (state string, ck *http.Cookie) {
	w := e.do("GET", "/login/oidc", nil, fromIP(ip))
	if w.Code != 303 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "http://127.0.0.1:8080/login/oidc/callback" {
		t.Fatalf("auth url: %s", loc)
	}
	ck = w.Result().Cookies()[0]
	if ck.Name != "siphon_oidc" || ck.Path != "/login/oidc" || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie %+v", ck)
	}
	base := map[string]any{"iss": p.URL, "aud": "cid", "sub": "u1", "email": "a@example.com", "email_verified": true,
		"nonce": q.Get("nonce"), "iat": e.now.Unix(), "exp": e.now.Add(time.Hour).Unix()}
	for k, v := range claims {
		if v == nil {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	p.claims = base
	return q.Get("state"), ck
}

func (e *env) callback(state string, ck *http.Cookie, ip string) *httptest.ResponseRecorder {
	return e.do("GET", "/login/oidc/callback?code="+testCode+"&state="+url.QueryEscape(state), nil, func(r *http.Request) {
		fromIP(ip)(r)
		r.AddCookie(ck)
	})
}

func (e *env) sessionOf(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge > 0 {
			return c
		}
	}
	return nil
}

func TestOIDCSignInRoles(t *testing.T) {
	p := newIdP(t)
	for _, tc := range []struct {
		name   string
		claims map[string]any
		want   role
	}{
		{"viewer by group", map[string]any{"groups": []string{"all"}}, roleViewer},
		{"operator by group string", map[string]any{"groups": "ops"}, roleOperator},
		{"highest wins", map[string]any{"groups": []string{"all", "adm", "ops"}}, roleAdmin},
		{"verified email, any case", map[string]any{"email": "v@example.COM"}, roleViewer},
		{"email_verified as a string", map[string]any{"email": "v@example.com", "email_verified": "true"}, roleViewer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := oidcEnv(t, p, nil)
			st, ck := e.begin(t, p, "192.0.2.10", tc.claims)
			w := e.callback(st, ck, "192.0.2.10")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `http-equiv="refresh" content="0;url=/"`) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %s %v", w.Code, w.Body, w.Header())
			}
			if p.verifier == "" {
				t.Error("no PKCE verifier sent")
			}
			if b := w.Body.String(); strings.Contains(b, testCode) || strings.Contains(b, p.lastToken) {
				t.Error("page echoes code or token")
			}
			c := e.sessionOf(w)
			if c == nil || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 43200 {
				t.Fatalf("session cookie %+v", c)
			}
			se, ok := e.srv.parseSession(c.Value)
			email, _ := tc.claims["email"].(string)
			if email == "" {
				email = "a@example.com"
			}
			if !ok || se.role != tc.want || se.actor != "oidc:"+email {
				t.Fatalf("session %+v %v", se, ok)
			}
			var n int
			e.st.DB.QueryRow(`SELECT count(*) FROM audit WHERE event='oidc_signin' AND actor=?`, "oidc:"+email).Scan(&n)
			if n != 1 {
				t.Errorf("audit rows %d", n)
			}
			if g := e.do("GET", "/rules", nil, func(r *http.Request) { r.AddCookie(c) }); g.Code != 200 || !strings.Contains(g.Body.String(), "oidc:"+email+" · "+tc.want.String()) {
				t.Errorf("rules page: %d", g.Code)
			}
		})
	}
}

// A role by group with an unverified email: the actor is the subject, never the email.
func TestOIDCUnverifiedEmailNotActor(t *testing.T) {
	p := newIdP(t)
	e := oidcEnv(t, p, nil)
	st, ck := e.begin(t, p, "192.0.2.13", map[string]any{"groups": []string{"ops"}, "email": "boss@example.com", "email_verified": false})
	c := e.sessionOf(e.callback(st, ck, "192.0.2.13"))
	if c == nil {
		t.Fatal("no session")
	}
	if se, ok := e.srv.parseSession(c.Value); !ok || se.actor != "oidc:u1" || se.role != roleOperator {
		t.Fatalf("session %+v %v", se, ok)
	}
}

func TestOIDCNoRoleRefused(t *testing.T) {
	p := newIdP(t)
	for name, claims := range map[string]map[string]any{
		"no match":         {"groups": []string{"other"}},
		"unverified email": {"email": "v@example.com", "email_verified": false},
		"missing verified": {"email": "v@example.com", "email_verified": nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := oidcEnv(t, p, nil)
			st, ck := e.begin(t, p, "192.0.2.11", claims)
			w := e.callback(st, ck, "192.0.2.11")
			if w.Code != 403 || e.sessionOf(w) != nil {
				t.Fatalf("%d", w.Code)
			}
			var n int
			e.st.DB.QueryRow(`SELECT count(*) FROM audit WHERE event='oidc_refused'`).Scan(&n)
			if n != 1 {
				t.Errorf("audit rows %d", n)
			}
		})
	}
}

func TestOIDCFailures(t *testing.T) {
	p := newIdP(t)
	for _, tc := range []struct {
		name   string
		claims map[string]any
		mutate func(e *env, st *string, ck **http.Cookie)
	}{
		{"wrong state", nil, func(_ *env, st *string, _ **http.Cookie) { *st = "nope" }},
		{"wrong nonce", map[string]any{"nonce": "other"}, nil},
		{"wrong aud", map[string]any{"aud": "someone-else"}, nil},
		{"wrong iss", map[string]any{"iss": "https://evil.example.com"}, nil},
		{"expired", map[string]any{"exp": time.Unix(1_800_000_000, 0).Add(-time.Hour).Unix()}, nil},
		{"bad signature", nil, func(*env, *string, **http.Cookie) { p.signWith = p.other }},
		{"old cookie", nil, func(e *env, _ *string, _ **http.Cookie) { e.now = e.now.Add(11 * time.Minute) }},
		{"tampered cookie", nil, func(_ *env, _ *string, ck **http.Cookie) {
			c := *(*ck)
			c.Value = strings.Replace(c.Value, "|", "|x", 1)
			*ck = &c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() { p.signWith = nil }()
			e := oidcEnv(t, p, nil)
			claims := map[string]any{"groups": []string{"adm"}}
			for k, v := range tc.claims {
				claims[k] = v
			}
			st, ck := e.begin(t, p, "192.0.2.12", claims)
			if tc.mutate != nil {
				tc.mutate(e, &st, &ck)
			}
			if tc.name == "old cookie" { // the ID token must still be fresh at the new time
				p.claims["iat"], p.claims["exp"] = e.now.Unix(), e.now.Add(time.Hour).Unix()
			}
			w := e.callback(st, ck, "192.0.2.12")
			if w.Code != 400 || e.sessionOf(w) != nil {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if b := w.Body.String(); strings.Contains(b, testCode) || (p.lastToken != "" && strings.Contains(b, p.lastToken)) {
				t.Error("page echoes code or token")
			}
		})
	}
}

func TestOIDCFailuresFeedLimiter(t *testing.T) {
	p := newIdP(t)
	e := oidcEnv(t, p, nil)
	for i := 0; i < failBurst; i++ {
		if w := e.callback("bad", &http.Cookie{Name: "siphon_oidc", Value: "x"}, "192.0.2.13"); w.Code != 400 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := e.callback("bad", &http.Cookie{Name: "siphon_oidc", Value: "x"}, "192.0.2.13"); w.Code != 429 {
		t.Fatalf("want 429, got %d", w.Code)
	}
}

func TestOIDCLoginPage(t *testing.T) {
	p := newIdP(t)
	off := false
	e := oidcEnv(t, p, nil)
	b := e.do("GET", "/login", nil, nil).Body.String()
	if !strings.Contains(b, "Sign in with SSO") || !strings.Contains(b, `name="token"`) {
		t.Error("want both SSO link and token form")
	}
	e = oidcEnv(t, p, func(o *config.OIDC) { o.TokenLogin = &off })
	b = e.do("GET", "/login", nil, nil).Body.String()
	if !strings.Contains(b, "Sign in with SSO") || strings.Contains(b, `name="token"`) {
		t.Error("token form should be hidden")
	}
	if w := newEnv(t, nil).do("GET", "/login/oidc", nil, nil); w.Code != 404 {
		t.Errorf("no oidc config: %d", w.Code)
	}
}

func TestOIDCProviderDown(t *testing.T) {
	p := newIdP(t)
	e := oidcEnv(t, p, nil)
	p.Close()
	w := e.do("GET", "/login/oidc", nil, nil)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "SSO provider unreachable") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// No sign-in cookie: refused, but not charged to the limiter (behind a proxy
// every user shares one bucket, and there is nothing to guess).
func TestOIDCNoCookieNotCounted(t *testing.T) {
	p := newIdP(t)
	e := oidcEnv(t, p, nil)
	for i := 0; i <= failBurst; i++ {
		w := e.do("GET", "/login/oidc/callback?state=x&code=y", nil, fromIP("192.0.2.14"))
		if w.Code != 400 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
}

// A failed discovery is reused for oidcRetry, then tried again.
func TestOIDCDiscoveryFailureCached(t *testing.T) {
	var hits atomic.Int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", 500)
	}))
	t.Cleanup(dead.Close)
	e := oidcEnv(t, &idp{Server: dead}, nil)
	for range 3 {
		if w := e.do("GET", "/login/oidc", nil, nil); w.Code != 502 {
			t.Fatalf("%d", w.Code)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("discovery hits %d, want 1", hits.Load())
	}
	e.now = e.now.Add(oidcRetry)
	e.do("GET", "/login/oidc", nil, nil)
	if hits.Load() != 2 {
		t.Fatalf("after retry window: %d", hits.Load())
	}
}
