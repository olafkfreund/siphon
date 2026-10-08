package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/store"
)

// M1: a connection name is unique across services.
func TestConnectionNameUniqueAcrossServices(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	if w := ce.post("/services/github", url.Values{"name": {"foo"}, "token": {"t"}}); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	rev := ce.latest()
	w := ce.post("/services/aws", url.Values{"name": {"foo"}, "region": {"eu-west-1"}, "mode": {"profile"}, "profile": {"p"}, "servers": {"cloudwatch"}})
	if w.Code != 422 || !strings.Contains(w.Body.String(), "a connection named foo already exists (service github)") || ce.latest() != rev {
		t.Fatalf("%d %s", w.Code, alertText(w.Body.String()))
	}
	if ce.cur.Load().Credentials["foo"] != nil {
		t.Error("credential created")
	}
}

// fakeLogin serves a GitHub-like /user and points catalogue tests at it.
func fakeLogin(t *testing.T) (cfg string) {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"login":"who"}`)) }))
	t.Cleanup(fake.Close)
	fu, _ := url.Parse(fake.URL)
	testTarget = func(u string) string { p, _ := url.Parse(u); return fake.URL + p.RequestURI() }
	t.Cleanup(func() { testTarget = func(u string) string { return u } })
	return strings.Replace(awsWebCfg, "server: { sandbox: none, db: DIR/s.db,", `server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["`+fu.Host+`"] },`, 1)
}

// M2: a tokenless ntfy connection offers no Test.
func TestTokenlessNtfyHasNoTest(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	ce.post("/services/ntfy", url.Values{"name": {"nt"}, "topic": {"alerts"}})
	ce.post("/services/ntfy", url.Values{"name": {"nt2"}, "topic": {"alerts"}, "token": {"tk_x"}})
	page := ce.get("/services").Body.String()
	if strings.Contains(page, "/services/c/nt/test") || !strings.Contains(page, "/services/c/nt2/test") {
		t.Error("Test offered wrongly")
	}
}

// M3: a legacy, unlabelled GitHub source can be tested.
func TestLegacyConnectionCanBeTested(t *testing.T) {
	t.Setenv("LEGACY_GH", "ghp_LEGACY")
	cfg := fakeLogin(t)
	cfg += "  lgh: { type: mcp, url: \"https://api.githubcopilot.com/mcp/\", auth: { bearer: \"env:LEGACY_GH\" }, read: { tool: get_me }, poll: 24h }\n"
	ce := newCfgEnvFile(t, cfg)
	if !strings.Contains(ce.get("/services").Body.String(), "/services/c/lgh/test") {
		t.Fatal("Test not offered")
	}
	w := ce.post("/services/c/lgh/test", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "who") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// L1: the Jira site must be an atlassian.net host, not a lookalike.
func TestJiraSitePattern(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	f := func(site string) url.Values {
		return url.Values{"name": {"jr"}, "site": {site}, "email": {"a@b.c"}, "token": {"tok"}}
	}
	for _, bad := range []string{"https://yourteam.atlassian.net.evil.example", "https://evil.example/yourteam.atlassian.net", "https://atlassian.net"} {
		if w := ce.post("/services/jira", f(bad)); w.Code != 422 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := ce.post("/services/jira", f("https://yourteam.atlassian.net/")); w.Code != 200 {
		t.Errorf("good site: %d %s", w.Code, alertText(w.Body.String()))
	}
}

// L2: Slack and Stripe issue the secret; the generic webhook won't invent one.
func TestGenericWebhookSlackStripeNeedsSecret(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	for _, mode := range []string{"slack", "stripe"} {
		w := ce.post("/services/webhook", url.Values{"name": {"w-" + mode}, "mode": {mode}})
		if w.Code != 422 || !strings.Contains(w.Body.String(), "paste it") {
			t.Errorf("%s: %d", mode, w.Code)
		}
		if w := ce.post("/services/webhook", url.Values{"name": {"w-" + mode}, "mode": {mode}, "secret": {"sig"}}); w.Code != 200 {
			t.Errorf("%s with secret: %d", mode, w.Code)
		}
	}
}

// L4: every form a token can leak in is masked.
func TestMaskParts(t *testing.T) {
	for tok, leaks := range map[string][]string{
		"Bearer SECRETTOK":                   {"SECRETTOK"},
		"Token token=SECRETTOK":              {"SECRETTOK"},
		"Basic bWFpbEB4LnlvOlNFQ1JFVFRPSw==": {"mail@x.yo", "SECRETTOK", "bWFpbEB4LnlvOlNFQ1JFVFRPSw=="},
	} {
		got := strings.Join(maskParts(tok), "|")
		for _, l := range leaks {
			if !strings.Contains(got, l) {
				t.Errorf("%q: %q not masked", tok, l)
			}
		}
	}
}

// L5, L7: a dotted name gets its result; a reused name starts Not checked.
func TestConnectionDottedNameAndReuse(t *testing.T) {
	ce := newCfgEnvFile(t, fakeLogin(t))
	ce.post("/services/github", url.Values{"name": {"a.b"}, "token": {"t"}})
	page := ce.get("/services").Body.String()
	if strings.Contains(page, "#conn-") || !strings.Contains(page, `hx-target="next .connection-result"`) {
		t.Error("hx-target not name-independent")
	}
	if w := ce.post("/services/c/a.b/test", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "who") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if c, _ := store.GetConnectionCheck(ce.st.DB, "a.b"); c == nil {
		t.Fatal("no check stored")
	}
	ce.api("DELETE", "/api/config/sources/a.b", "")
	ce.post("/services/github", url.Values{"name": {"a.b"}, "token": {"t"}})
	if c, _ := store.GetConnectionCheck(ce.st.DB, "a.b"); c != nil {
		t.Errorf("stale check %+v", c)
	}
}

// L6: secrets are trimmed, and a line break is refused.
func TestSecretTrimmedAndSingleLine(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	if w := ce.post("/services/github", url.Values{"name": {"tr"}, "token": {"  ghp_ok\n"}}); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	if v := ce.cur.Load().Sources["tr"].Auth.Bearer.Value; v != "ghp_ok" {
		t.Errorf("stored %q", v)
	}
	if w := ce.post("/services/github", url.Values{"name": {"tr2"}, "token": {"ghp\nX-Evil: 1"}}); w.Code != 422 {
		t.Errorf("newline: %d", w.Code)
	}
}
