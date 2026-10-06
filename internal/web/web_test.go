package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

const tok = "s3cret-token"

type env struct {
	t   *testing.T
	st  *store.Store
	h   http.Handler
	now time.Time
}

func newEnv(t *testing.T, mod func(*Options)) *env {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Unix(1_800_000_000, 0)}
	o := Options{
		Token: tok, Store: st, Now: func() time.Time { return e.now },
		Cfg: &config.Config{
			Sources: map[string]*config.Source{"disk": {Type: "http", Poll: config.Duration(time.Minute)}, "gh": {Type: "webhook"}},
			Rules: []config.Rule{{Name: "disk-full", Source: "disk", When: "true", Action: config.Action{Unit: "nix-gc.service"}},
				{Name: "other", Source: "disk", When: "true", Action: config.Action{Cmd: []string{"true"}}}},
		},
	}
	if mod != nil {
		mod(&o)
	}
	e.h = New(o)
	return e
}

func (e *env) do(method, path string, body url.Values, mod func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.RemoteAddr = "192.0.2.1:1234"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func bearer(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }

// login returns the session cookie and the CSRF token scraped from a page.
func (e *env) login() (*http.Cookie, string) {
	w := e.do("POST", "/login", url.Values{"token": {tok}}, nil)
	if w.Code != 303 {
		e.t.Fatalf("login: %d", w.Code)
	}
	c := w.Result().Cookies()[0]
	w = e.do("GET", "/rules", nil, func(r *http.Request) { r.AddCookie(c) })
	m := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(w.Body.String())
	if m == nil {
		e.t.Fatalf("no csrf in page: %s", w.Body.String())
	}
	return c, m[1]
}

func (e *env) pendingJob() int64 {
	tx, _ := e.st.DB.Begin()
	id, err := store.InsertJob(tx, store.Job{Rule: "r", ActionJSON: "{}", State: "pending_approval"}, e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := store.CreateApproval(tx, id, make([]byte, 32), e.now.Add(time.Hour)); err != nil {
		e.t.Fatal(err)
	}
	tx.Commit()
	return id
}

func TestAPIAuth(t *testing.T) {
	e := newEnv(t, nil)
	if w := e.do("GET", "/api/rules", nil, nil); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := e.do("GET", "/api/rules", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }); w.Code != 401 {
		t.Fatalf("bad token: %d", w.Code)
	}
	for _, p := range []string{"/api/sources", "/api/rules", "/api/jobs", "/api/approvals", "/api/audit"} {
		w := e.do("GET", p, nil, bearer)
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "[") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "/api/jobs/999", nil, bearer); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestEmptyTokenLocksEverythingOut(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Token = "" })
	if w := e.do("GET", "/api/rules", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestHealthzNoAuth(t *testing.T) {
	e := newEnv(t, nil)
	if w := e.do("GET", "/healthz", nil, nil); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAPIApproveWithFakeDecide(t *testing.T) {
	type call struct {
		id      int64
		approve bool
		by      string
	}
	var calls []call
	e := newEnv(t, func(o *Options) {
		o.Decide = func(id int64, a bool, by string) error { calls = append(calls, call{id, a, by}); return nil }
	})
	if w := e.do("POST", "/api/jobs/7/approve", nil, bearer); w.Code != 200 {
		t.Fatal(w.Code)
	}
	e.do("POST", "/api/jobs/8/deny", nil, bearer)
	if len(calls) != 2 || calls[0] != (call{7, true, "api"}) || calls[1] != (call{8, false, "api"}) {
		t.Fatalf("%+v", calls)
	}
	if w := e.do("POST", "/api/jobs/7/approve", nil, nil); w.Code != 401 || len(calls) != 2 {
		t.Fatal("POST needs auth")
	}
}

func TestAPIApproveChangesState(t *testing.T) {
	e := newEnv(t, nil)
	id := e.pendingJob()
	if w := e.do("GET", "/api/approvals", nil, bearer); !strings.Contains(w.Body.String(), `"job_id":`+itoa(int(id))) {
		t.Fatal(w.Body.String())
	}
	if w := e.do("POST", "/api/jobs/"+itoa(int(id))+"/approve", nil, bearer); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var j store.JobDetail
	w := e.do("GET", "/api/jobs/"+itoa(int(id)), nil, bearer)
	json.Unmarshal(w.Body.Bytes(), &j)
	if j.State != "queued" {
		t.Fatalf("state %s", j.State)
	}
	if w := e.do("POST", "/api/jobs/"+itoa(int(id))+"/approve", nil, bearer); w.Code != 409 {
		t.Fatalf("second decision: %d", w.Code)
	}
	if !strings.Contains(e.do("GET", "/api/audit", nil, bearer).Body.String(), `"actor":"api"`) {
		t.Fatal("decision not audited")
	}
}

func TestAPIRuleOverride(t *testing.T) {
	e := newEnv(t, nil)
	if w := e.do("POST", "/api/rules/disk-full/disable", nil, bearer); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var rules []ruleView
	json.Unmarshal(e.do("GET", "/api/rules", nil, bearer).Body.Bytes(), &rules)
	if rules[0].Enabled || !rules[0].Overridden || !rules[1].Enabled {
		t.Fatalf("%+v", rules)
	}
	tx, _ := e.st.DB.Begin()
	en, ov, _ := store.RuleEnabled(tx, "disk-full")
	tx.Rollback()
	if en || !ov {
		t.Fatal("store.RuleEnabled disagrees")
	}
	e.do("POST", "/api/rules/disk-full/enable", nil, bearer)
	json.Unmarshal(e.do("GET", "/api/rules", nil, bearer).Body.Bytes(), &rules)
	if !rules[0].Enabled || rules[0].Overridden {
		t.Fatalf("%+v", rules)
	}
	if w := e.do("POST", "/api/rules/ghost/disable", nil, bearer); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestPortalRequiresLogin(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/jobs", nil, nil)
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatal(w.Code, w.Header())
	}
	if w := e.do("POST", "/rules/disk-full/disable", url.Values{}, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestLoginCookieAttributes(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("POST", "/login", url.Values{"token": {tok}}, nil)
	c := w.Result().Cookies()[0]
	if c.Name != "agentgw_session" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || strings.Contains(c.Value, tok) {
		t.Fatalf("%+v", c)
	}
	w = e.do("POST", "/login", url.Values{"token": {tok}}, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") })
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("Secure expected behind https proxy")
	}
}

func TestPortalPostWithoutCSRFIs403(t *testing.T) {
	e := newEnv(t, nil)
	c, csrf := e.login()
	add := func(r *http.Request) { r.AddCookie(c) }
	if w := e.do("POST", "/rules/disk-full/disable", url.Values{}, add); w.Code != 403 {
		t.Fatalf("no csrf: %d", w.Code)
	}
	if w := e.do("POST", "/rules/disk-full/disable", url.Values{"csrf": {"bad"}}, add); w.Code != 403 {
		t.Fatalf("bad csrf: %d", w.Code)
	}
	if w := e.do("POST", "/rules/disk-full/disable", url.Values{"csrf": {csrf}}, add); w.Code != 303 {
		t.Fatalf("good csrf: %d", w.Code)
	}
	// htmx path: header token, partial in response
	w := e.do("POST", "/rules/other/disable", url.Values{}, func(r *http.Request) {
		add(r)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("HX-Request", "true")
	})
	if w.Code != 200 || strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("htmx partial: %d %s", w.Code, w.Body.String())
	}
}

func TestPortalShowsOverridden(t *testing.T) {
	e := newEnv(t, nil)
	c, csrf := e.login()
	e.do("POST", "/rules/disk-full/disable", url.Values{"csrf": {csrf}}, func(r *http.Request) { r.AddCookie(c) })
	body := e.do("GET", "/rules", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if !strings.Contains(body, "overridden") || !strings.Contains(body, "Enable") {
		t.Fatal(body)
	}
}

func TestPortalPagesAndApprovalButton(t *testing.T) {
	e := newEnv(t, nil)
	c, csrf := e.login()
	add := func(r *http.Request) { r.AddCookie(c) }
	id := e.pendingJob()
	for _, p := range []string{"/sources", "/rules", "/jobs", "/jobs/" + itoa(int(id)), "/approvals", "/audit"} {
		w := e.do("GET", p, nil, add)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<main") {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	if w := e.do("GET", "/jobs/999", nil, add); w.Code != 404 {
		t.Fatal(w.Code)
	}
	w := e.do("POST", "/approvals/"+itoa(int(id))+"/deny", url.Values{"csrf": {csrf}}, add)
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	j, _ := store.GetJob(e.st.DB, id)
	if j.State != "cancelled" {
		t.Fatal(j.State)
	}
}

func TestPortalEscapesOutput(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.login()
	tx, _ := e.st.DB.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "r", ActionJSON: "{}"}, e.now)
	tx.Commit()
	e.st.DB.Exec(`UPDATE jobs SET output='<script>alert(1)</script>' WHERE id=?`, id)
	body := e.do("GET", "/jobs/"+itoa(int(id)), nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if strings.Contains(body, "<script>alert") {
		t.Fatal("output not escaped")
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/login", "/healthz", "/static/style.css"} {
		h := e.do("GET", p, nil, nil).Header()
		if h.Get("Content-Security-Policy") != "default-src 'self'" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: %v", p, h)
		}
	}
	if w := e.do("GET", "/static/htmx.min.js", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "htmx 2.0.4") {
		t.Fatal("htmx not served")
	}
}

func TestBadLoginRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	for i := 1; i <= 5; i++ {
		if w := e.do("POST", "/login", url.Values{"token": {"bad"}}, nil); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := e.do("POST", "/login", url.Values{"token": {"bad"}}, nil); w.Code != 429 {
		t.Fatalf("6th: %d", w.Code)
	}
	if w := e.do("POST", "/login", url.Values{"token": {tok}}, nil); w.Code != 429 {
		t.Fatal("blocked IP stays blocked even with the right token")
	}
	if w := e.do("GET", "/api/rules", nil, bearer); w.Code != 429 {
		t.Fatal("API shares the bucket")
	}
	other := func(r *http.Request) { r.RemoteAddr = "198.51.100.9:1" }
	if w := e.do("POST", "/login", url.Values{"token": {tok}}, other); w.Code != 303 {
		t.Fatal("other IPs unaffected")
	}
	e.now = e.now.Add(time.Minute)
	if w := e.do("POST", "/login", url.Values{"token": {tok}}, nil); w.Code != 303 {
		t.Fatalf("after refill: %d", w.Code)
	}
}

func TestAPIAuthFailuresRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 5; i++ {
		e.do("GET", "/api/rules", nil, nil)
	}
	if w := e.do("GET", "/api/rules", nil, nil); w.Code != 429 {
		t.Fatal(w.Code)
	}
}

func TestHooksUnauthenticated(t *testing.T) {
	hit := ""
	e := newEnv(t, func(o *Options) {
		o.Hooks = map[string]http.Handler{"gh": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = r.PathValue("source"); w.WriteHeader(202) })}
	})
	if w := e.do("POST", "/hook/gh", nil, nil); w.Code != 202 || hit != "gh" {
		t.Fatal(w.Code, hit)
	}
	if w := e.do("POST", "/hook/zz", nil, nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
