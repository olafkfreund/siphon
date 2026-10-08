package web

import (
	"encoding/base64"
	"io"
	"time"

	"encoding/json"
	"github.com/olafkfreund/siphon/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/catalog"
)

// Every catalogue entry renders with dummy values and passes the normal
// validated edit path; no secret value ever reaches the rendered YAML.
func TestCatalogEntriesRenderAndCommit(t *testing.T) {
	const sentinel = "SENTINEL-SECRET-VALUE"
	ce := newCfgEnvFile(t, strings.Replace(awsWebCfg, "mcp_packages: {", "mcp_packages: {\n  github: {command: [gh], hosts: [api.github.com]},"+catalogPackagesYAML(), 1))
	for _, e := range catalog.All() {
		if e.Status != "available" {
			continue
		}
		v := dummyValues(e, sentinel)
		res, err := e.Render(v, catalog.Env{Config: ce.cur.Load()})
		if err != nil {
			t.Fatalf("%s: %v", e.ID, err)
		}
		for _, it := range res.Items {
			if strings.Contains(it.YAML, sentinel) || !strings.Contains(it.YAML, "connection: \"t-"+e.ID+"\"") {
				t.Errorf("%s/%s yaml:\n%s", e.ID, it.Name, it.YAML)
			}
		}
		form := url.Values{}
		for k, x := range v {
			form[k] = strings.Split(x, ",")
		}
		if w := ce.post("/services/"+e.ID, form); w.Code != 200 {
			t.Errorf("%s: commit: %d %s", e.ID, w.Code, alertText(w.Body.String()))
		}
		if ce.cur.Load().Sources["t-"+e.ID] == nil && ce.cur.Load().Credentials["t-"+e.ID] == nil {
			t.Errorf("%s: nothing created", e.ID)
		}
	}
}

func TestCatalogSecretsNotInHistory(t *testing.T) {
	ce := newCfgEnv(t)
	ce.post("/services/github", url.Values{"name": {"gh7"}, "token": {"SENTINEL-HIST"}, "webhook": {"on"}})
	for _, p := range []string{"/history", "/history/export", "/config/sources/gh7"} {
		if strings.Contains(ce.get(p).Body.String(), "SENTINEL-HIST") {
			t.Errorf("secret on %s", p)
		}
	}
}

func TestCatalogAPIAvailabilityAndGenericConnect(t *testing.T) {
	ce := newCfgEnv(t) // no mcp_packages: aws needs a package
	w := ce.api("GET", "/api/catalog", "")
	var cat struct {
		Services []struct {
			ID, Availability, Reason string
			AvailabilityReason       string `json:"availability_reason"`
			Fields                   []struct{ Key string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cat); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	got := map[string]string{}
	for _, s := range cat.Services {
		got[s.ID] = s.Availability
		if s.ID == "aws" && !strings.Contains(s.AvailabilityReason, "services.siphon.aws") {
			t.Errorf("aws reason: %q", s.AvailabilityReason)
		}
	}
	if got["github"] != "available" || got["aws"] != "needs-package" {
		t.Fatalf("availability %v", got)
	}
	if strings.Contains(w.Body.String(), "yaml") || strings.Contains(w.Body.String(), "{{") {
		t.Error("templates leaked")
	}

	// Generic connect: {"fields": {...}}, no-store, one-time secret.
	w = ce.api("POST", "/api/services/github", jbody(map[string]any{"fields": map[string]any{"name": "g1", "token": "ghp_GEN", "webhook": true}}))
	var done serviceDone
	json.Unmarshal(w.Body.Bytes(), &done)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || len(done.HookSecret) != 64 || strings.Contains(w.Body.String(), "ghp_GEN") {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	if c := ce.cur.Load().Sources["g1"]; c == nil || c.Connection != "g1" {
		t.Fatal("not connected / not labelled")
	}
	if w := ce.api("POST", "/api/services/nope", `{}`); w.Code != 404 {
		t.Errorf("unknown: %d", w.Code)
	}
	if w := ce.api("POST", "/api/services/aws", jbody(map[string]any{"name": "aws", "region": "eu-west-1", "profile": "p", "servers": []string{"docs"}})); w.Code != 422 || !strings.Contains(w.Body.String(), "services.siphon.aws") {
		t.Errorf("unavailable: %d %s", w.Code, w.Body.String())
	}
}

func TestConnectionTest(t *testing.T) {
	const tok = "glpat-SECRET9"
	mode := "ok"
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/api/v4/user" || r.Header.Get("PRIVATE-TOKEN") != tok:
			http.Error(w, "nope "+tok, 401)
		case mode == "deny":
			http.Error(w, "no", 401)
		case mode == "echo":
			w.Write([]byte(`{"username":"` + tok + `"}`))
		default:
			w.Write([]byte(`{"username":"olaf"}`))
		}
	}))
	defer gl.Close()
	u, _ := url.Parse(gl.URL)
	ce := newCfgEnvFile(t, strings.Replace(cfgFile, "server: { sandbox: none, db: DIR/s.db }",
		`server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["`+u.Host+`"] } }`, 1))
	if w := ce.api("POST", "/api/connections/gl/test", ""); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	ce.api("POST", "/api/services/gitlab", jbody(map[string]any{"name": "gl", "base": gl.URL, "project": "g/p", "token": tok}))
	check := func(wantOK bool, wantDetail string) {
		t.Helper()
		w := ce.api("POST", "/api/connections/gl/test", "")
		var r struct {
			OK     bool
			Detail string
		}
		json.Unmarshal(w.Body.Bytes(), &r)
		if w.Code != 200 || r.OK != wantOK || !strings.Contains(r.Detail, wantDetail) || strings.Contains(w.Body.String(), tok) {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var ok bool
		var detail string
		ce.st.DB.QueryRow(`SELECT ok, detail FROM connection_check WHERE connection='gl'`).Scan(&ok, &detail)
		if ok != wantOK || strings.Contains(detail, tok) || !strings.Contains(detail, wantDetail) {
			t.Fatalf("stored %v %q", ok, detail)
		}
	}
	check(true, "olaf")
	mode = "echo" // the identity is the token itself: it is masked
	check(true, "***")
	mode = "deny"
	check(false, "refused")
}

func TestServicesPageConnectedGroupingAndStatus(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg) // gh: a legacy, unlabelled GitHub webhook
	ce.post("/services/github", url.Values{"name": {"work"}, "token": {"ghp_X"}, "webhook": {"on"}})
	ce.post("/services/aws", url.Values{"name": {"prod"}, "region": {"eu-west-1"}, "mode": {"profile"}, "profile": {"p"}, "servers": {"cloudwatch", "docs"}, "webhook": {"on"}})
	page := ce.get("/services").Body.String()
	for _, want := range []string{"GitHub · work", "GitHub · gh", "AWS · prod"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	// One row per connection, not per source.
	if n := strings.Count(page, `class="connection-row"`); n != 3 {
		t.Errorf("rows: %d", n)
	}
	for _, link := range []string{"/config/sources/work-hooks", "/config/sources/prod-docs", "/config/credentials/prod"} {
		if !strings.Contains(page, link) {
			t.Errorf("no edit link %s", link)
		}
	}
	if strings.Count(page, "status-unchecked") != 3 || !strings.Contains(page, "Last event: No events yet") {
		t.Error("expected Not checked everywhere")
	}
	store.SetConnectionCheck(ce.st.DB, store.ConnectionCheck{Connection: "work", At: time.Now(), OK: true, Detail: "olaf"})
	store.SetConnectionCheck(ce.st.DB, store.ConnectionCheck{Connection: "prod", At: time.Now(), OK: false, Detail: "keys refused"})
	page = ce.get("/services").Body.String()
	if !strings.Contains(page, "status-working") || !strings.Contains(page, "status-attention") || !strings.Contains(page, "keys refused") || strings.Count(page, "status-unchecked") != 1 {
		t.Errorf("status pills wrong")
	}
	// Test is offered where the service has one (GitHub, AWS), not elsewhere.
	if !strings.Contains(page, "/services/c/work/test") || !strings.Contains(page, "/services/c/prod/test") {
		t.Error("Test missing")
	}
}

func TestServicesExploreSearchAndFilter(t *testing.T) {
	ce := newCfgEnv(t)
	full := ce.get("/services").Body.String()
	for _, want := range []string{`id="service-results"`, "<html", "Explore services", `href="/services/github"`} {
		if !strings.Contains(full, want) {
			t.Errorf("full page lacks %q", want)
		}
	}
	// aws is unavailable here: its reason shows and it has no Connect.
	if strings.Contains(full, `href="/services/aws"`) || !strings.Contains(full, "services.siphon.aws") {
		t.Error("unavailable tile offers Connect or hides its reason")
	}
	q := ce.get("/services?q=merge").Body.String()
	if !strings.Contains(q, "GitLab") || strings.Contains(q, `href="/services/github"`) {
		t.Error("search")
	}
	c := ce.get("/services?cat=cloud").Body.String()
	if !strings.Contains(c, `aria-current="true"`) || strings.Contains(c, `href="/services/gitlab"`) {
		t.Error("category filter")
	}
	if none := ce.get("/services?q=zzzz").Body.String(); !strings.Contains(none, "No services match") {
		t.Error("empty state")
	}
	// htmx gets the page without the layout; the grid is inside it.
	req := httptest.NewRequest("GET", "/services?q=merge", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(ce.c)
	rec := httptest.NewRecorder()
	ce.h.ServeHTTP(rec, req)
	if b := rec.Body.String(); rec.Code != 200 || strings.Contains(b, "<html") || !strings.Contains(b, `id="service-results"`) || !strings.Contains(b, "GitLab") {
		t.Errorf("htmx: %d", rec.Code)
	}
}

func TestServiceConnectPageErrorsKeepValues(t *testing.T) {
	ce := newCfgEnv(t)
	form := ce.get("/services/gitlab").Body.String()
	for _, want := range []string{"1</span>What to connect", "2</span>Access", "3</span>Events", `class="check toggle-row"`, `type="password"`} {
		if !strings.Contains(form, want) {
			t.Errorf("connect page lacks %q", want)
		}
	}
	if ce.get("/services/nope").Code != 404 {
		t.Error("unknown service")
	}
	w := ce.post("/services/gitlab", url.Values{"name": {"mygl"}, "base": {"http://gitlab.lan"}, "project": {"grp/proj"}, "token": {"glpat-NEVER"}, "webhook": {"on"}})
	b := w.Body.String()
	if w.Code != 422 || !strings.Contains(b, "must be https") || !strings.Contains(b, `value="mygl"`) || !strings.Contains(b, `value="grp/proj"`) || !strings.Contains(b, `value="http://gitlab.lan"`) {
		t.Fatalf("%d %s", w.Code, b)
	}
	if strings.Contains(b, "glpat-NEVER") {
		t.Error("secret echoed")
	}
	// The error sits inside the field it belongs to.
	i := strings.Index(b, "must be https")
	if j := strings.LastIndex(b[:i], "<label"); j < 0 || !strings.Contains(b[j:i], `name="base"`) {
		t.Error("error not inline with the URL field")
	}
	if !strings.Contains(b, "checked") {
		t.Error("webhook choice lost")
	}
}

func TestWebhookOnlyConnectionHasNoTest(t *testing.T) {
	ce := newCfgEnv(t)
	ce.post("/services/github", url.Values{"name": {"wk"}, "token": {"t"}, "webhook": {"on"}})
	// Keep only the webhook half: the connection then has no token to test with.
	ce.api("DELETE", "/api/config/sources/wk", "")
	page := ce.get("/services").Body.String()
	if !strings.Contains(page, "GitHub · wk") || strings.Contains(page, "/services/c/wk/test") || !strings.Contains(page, "send an event to check") {
		t.Errorf("webhook-only row: test offered or hint missing")
	}
	if !strings.Contains(ce.get("/services").Body.String(), "<code>services.siphon.aws</code>") {
		t.Error("reason not rendered as code")
	}
}

// catalogPackagesYAML is mcp_packages entries for every needs-package service,
// with the env names the catalogue says they take.
func catalogPackagesYAML() string {
	var b strings.Builder
	for _, e := range catalog.All() {
		if e.Nix != nil {
			b.WriteString("\n  " + e.ID + ": {command: [x], env: [" + strings.Join(e.Nix.Env, ", ") + "], url_env: [" + strings.Join(e.Nix.URLEnv, ", ") + "]},")
		}
	}
	return b.String()
}

func TestNotYetServicesCannotConnect(t *testing.T) {
	ce := newCfgEnv(t)
	n := 0
	for _, e := range catalog.All() {
		if e.Status != "not-yet" {
			continue
		}
		n++
		w := ce.api("POST", "/api/services/"+e.ID, jbody(map[string]any{"fields": map[string]any{"name": "x"}}))
		if w.Code != 422 || !strings.Contains(w.Body.String(), "can't be connected here") {
			t.Errorf("%s: %d %s", e.ID, w.Code, w.Body.String())
		}
		if p := ce.get("/services/" + e.ID).Body.String(); strings.Contains(p, `class="connect-form"`) || !strings.Contains(p, "can't be connected") {
			t.Errorf("%s: connect page offers a form", e.ID)
		}
	}
	if n < 9 {
		t.Errorf("only %d not-yet entries", n)
	}
}

// alertText is the error shown on a portal page, for readable failures.
func alertText(page string) string {
	if i := strings.Index(page, `role="alert"`); i >= 0 {
		return page[i:min(len(page), i+300)]
	}
	return page[:min(len(page), 300)]
}

// dummyValues fills every field of an entry with something valid.
func dummyValues(e *catalog.Entry, sentinel string) map[string]string {
	v := map[string]string{"name": "t-" + e.ID, "project": "g/p", "profile": "p", "webhook": "on", "region": "eu-west-1"}
	for _, f := range e.Fields {
		if _, ok := v[f.Key]; ok {
			continue
		}
		switch f.Type {
		case "secret":
			v[f.Key] = sentinel
		case "multi":
			v[f.Key] = strings.Join(f.Choices, ",")
		case "url":
			v[f.Key] = "https://example.com"
			if e.ID == "jira" {
				v[f.Key] = "https://t.atlassian.net"
			}
		case "choice":
			v[f.Key] = f.Choices[0]
		default:
			if f.Default == "" {
				v[f.Key] = "dummy"
			}
		}
	}
	if e.ID == "aws" { // profile mode: no base keys
		delete(v, "access_key_id")
		delete(v, "secret_access_key")
	}
	return v
}

// Each entry's test runs against a fake server: the right method, the
// credential in the right header, and the identity read from the JSON path.
func TestCatalogEntryTestsAgainstFakeServer(t *testing.T) {
	var got struct {
		method, path, body string
		hdr                http.Header
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.body, got.hdr = r.Method, r.URL.Path, string(b), r.Header.Clone()
		w.Write([]byte(`{"login":"who","username":"who","display_name":"who","displayName":"who","object":"who","message":"who",
			"data":{"viewer":{"name":"who"}},"result":{"status":"who"}}`))
	}))
	defer fake.Close()
	fu, _ := url.Parse(fake.URL)
	testTarget = func(u string) string {
		p, _ := url.Parse(u)
		return fake.URL + p.RequestURI()
	}
	defer func() { testTarget = func(u string) string { return u } }()
	cfg := strings.Replace(awsWebCfg, "server: { sandbox: none, db: DIR/s.db,", `server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["`+fu.Host+`"] },`, 1)
	ce := newCfgEnvFile(t, strings.Replace(cfg, "mcp_packages: {", "mcp_packages: {"+catalogPackagesYAML(), 1))
	tested := 0
	for _, e := range catalog.All() {
		if e.Status != "available" || e.Test == nil {
			continue
		}
		tested++
		v := dummyValues(e, "SENTINEL-TEST-KEY")
		w := ce.api("POST", "/api/services/"+e.ID, jbody(map[string]any{"fields": v}))
		if w.Code != 200 {
			t.Errorf("%s: connect %d %s", e.ID, w.Code, w.Body.String())
			continue
		}
		got.method = ""
		w = ce.api("POST", "/api/connections/t-"+e.ID+"/test", "")
		var r struct {
			OK     bool
			Detail string
		}
		json.Unmarshal(w.Body.Bytes(), &r)
		if w.Code != 200 || !r.OK || r.Detail != "who" {
			t.Errorf("%s: test %d %s", e.ID, w.Code, w.Body.String())
			continue
		}
		if got.method != catalog.OrGET(e.Test.Method) {
			t.Errorf("%s: method %q", e.ID, got.method)
		}
		name, _, _ := strings.Cut(e.Test.Header, ":")
		// the exact credential: the stored value, sent once (no double prefix)
		_, tmpl, _ := strings.Cut(e.Test.Header, ":")
		stored := "SENTINEL-TEST-KEY"
		switch e.ID {
		case "ntfy":
			stored = "Bearer " + stored
		case "bitbucket", "jira":
			stored = "Basic " + base64.StdEncoding.EncodeToString([]byte(v["email"]+":"+stored))
		}
		if want, g := strings.TrimSpace(strings.ReplaceAll(tmpl, "{token}", stored)), got.hdr.Get(strings.TrimSpace(name)); g != want {
			t.Errorf("%s: header %s = %q, want %q", e.ID, name, g, want)
		}
		if e.Test.Body != "" && got.body != e.Test.Body {
			t.Errorf("%s: body %q", e.ID, got.body)
		}
		// a refused credential is reported, not hidden
		if strings.Contains(w.Body.String(), "SENTINEL-TEST-KEY") {
			t.Errorf("%s: secret in the reply", e.ID)
		}
	}
	if tested < 10 {
		t.Errorf("only %d entries have a test", tested)
	}
}

func TestLocalPackageServiceRefusesUnlistedPrivateHost(t *testing.T) {
	ce := newCfgEnvFile(t, strings.Replace(awsWebCfg, "mcp_packages: {", "mcp_packages: {"+catalogPackagesYAML(), 1))
	body := func(base string) string {
		return jbody(map[string]any{"fields": map[string]any{"name": "gt", "base": base, "token": "t"}})
	}
	for _, base := range []string{"http://10.0.0.5:3000", "https://localhost", "https://192.168.1.2"} {
		if w := ce.api("POST", "/api/services/gitea", body(base)); w.Code != 422 || !strings.Contains(w.Body.String(), "private_endpoints") {
			t.Errorf("%s: %d %s", base, w.Code, w.Body.String())
		}
	}
	if ce.cur.Load().Sources["gt"] != nil {
		t.Fatal("created")
	}
	if w := ce.api("POST", "/api/services/gitea", body("https://git.example.org")); w.Code != 200 {
		t.Errorf("public: %d %s", w.Code, w.Body.String())
	}
}

func TestLocalPackageServiceAllowsListedPrivateHost(t *testing.T) {
	cfg := strings.Replace(awsWebCfg, "server: { sandbox: none, db: DIR/s.db,", `server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["10.0.0.5:3000"] },`, 1)
	ce := newCfgEnvFile(t, strings.Replace(cfg, "mcp_packages: {", "mcp_packages: {"+catalogPackagesYAML(), 1))
	w := ce.api("POST", "/api/services/gitea", jbody(map[string]any{"fields": map[string]any{"name": "gt", "base": "http://10.0.0.5:3000", "token": "t"}}))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	c := ce.cur.Load()
	if err := c.BridgeCheck(c.Sources["gt"]); err != nil {
		t.Error(err)
	}
	if hp := c.BridgeEgress(c.Sources["gt"]); len(hp) != 1 || !hp[0].AllowPrivate {
		t.Errorf("egress %v", hp)
	}
}
