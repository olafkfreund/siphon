package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

const cfgFile = `server: { sandbox: none, db: DIR/s.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
rules:
  - { name: r1, source: gh, when: "true", action: { cmd: [echo, one] } }
`

type cfgEnv struct {
	*env
	dir       string
	cur       atomic.Pointer[config.Config]
	applied   atomic.Int32
	testAWS   func(context.Context, string) (string, time.Time, []string, error)
	applyFail atomic.Bool // makes Apply fail (the save still lands)
	c         *http.Cookie
	keep      int // overlay rows the security tests expect to remain
	csrf      string
}

func newCfgEnv(t *testing.T) *cfgEnv { return newCfgEnvFile(t, cfgFile) }

func newCfgEnvFile(t *testing.T, content string) *cfgEnv {
	t.Helper()
	t.Setenv("AGW_HOOK", "s3cret")
	ce := &cfgEnv{dir: t.TempDir()}
	path := filepath.Join(ce.dir, "siphon.yaml")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(content, "DIR", ce.dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadWithOverlay(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ce.cur.Store(cfg)
	ce.env = newEnv(t, func(o *Options) {
		o.Config = ce.cur.Load
		o.Apply = func(c *config.Config) error {
			if ce.applyFail.Load() {
				return errors.New("apply boom")
			}
			ce.cur.Store(c)
			ce.applied.Add(1)
			return nil
		}
		o.ConfigPath = path
		o.TestAWS = func(ctx context.Context, src string) (string, time.Time, []string, error) {
			if ce.testAWS == nil {
				return "", time.Time{}, nil, errors.New("no TestAWS stub")
			}
			return ce.testAWS(ctx, src)
		}
	})
	ce.c, ce.csrf = ce.login()
	return ce
}

func (ce *cfgEnv) latest() int64 {
	r, _, _ := store.LatestRevision(ce.st.DB)
	return r.ID
}

// post sends a portal form POST with the session and CSRF token.
func (ce *cfgEnv) post(path string, v url.Values) *httptest.ResponseRecorder {
	if v == nil {
		v = url.Values{}
	}
	if v.Get("csrf") == "" {
		v.Set("csrf", ce.csrf)
	}
	return ce.do("POST", path, v, func(r *http.Request) { r.AddCookie(ce.c) })
}

func (ce *cfgEnv) get(path string) *httptest.ResponseRecorder {
	return ce.do("GET", path, nil, func(r *http.Request) { r.AddCookie(ce.c) })
}

func (ce *cfgEnv) saveYAML(kind, name, y string) *httptest.ResponseRecorder {
	return ce.post("/config/"+kind+"/"+name+"/save", url.Values{"mode": {"yaml"}, "name": {name}, "yaml": {y}, "rev": {strconv.FormatInt(ce.latest(), 10)}})
}

func (ce *cfgEnv) act(kind, name, verb string) *httptest.ResponseRecorder {
	return ce.post("/config/"+kind+"/"+name+"/"+verb, url.Values{"rev": {strconv.FormatInt(ce.latest(), 10)}})
}

func (ce *cfgEnv) rules() (names []string) {
	for _, r := range ce.cur.Load().Rules {
		names = append(names, r.Name)
	}
	return
}

func (ce *cfgEnv) api(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	ce.h.ServeHTTP(w, r)
	return w
}

func TestConfigCRUDEveryKind(t *testing.T) {
	ce := newCfgEnv(t)
	items := []struct{ kind, name, yaml, yaml2 string }{
		{"rules", "r2", `{source: gh, when: "true", action: {cmd: [echo, two]}}`, `{source: gh, when: "false", action: {cmd: [echo, two]}}`},
		{"sources", "s2", `{type: http, url: "https://example.com/x", poll: 1m}`, `{type: http, url: "https://example.com/x", poll: 2m, method: POST}`},
		{"agents", "a1", `{kind: claude, prompt: hi}`, `{kind: claude, prompt: hello}`},
		{"routines", "ro", `{steps: [{id: s1, cmd: [echo, x]}]}`, `{steps: [{id: s1, cmd: [echo, y]}]}`},
		{"credentials", "c1", `{provider: claude}`, `{provider: claude, concurrency: 2}`},
	}
	for _, it := range items {
		if w := ce.saveYAML(it.kind, it.name, it.yaml); w.Code != 303 {
			t.Fatalf("create %s/%s: %d %s", it.kind, it.name, w.Code, w.Body.String())
		}
		if w := ce.saveYAML(it.kind, it.name, it.yaml2); w.Code != 303 {
			t.Fatalf("update %s/%s: %d %s", it.kind, it.name, w.Code, w.Body.String())
		}
		if w := ce.get("/config/" + it.kind + "/" + it.name); w.Code != 200 || !strings.Contains(w.Body.String(), it.name) {
			t.Fatalf("edit page %s: %d", it.kind, w.Code)
		}
		if w := ce.act(it.kind, it.name, "delete"); w.Code != 303 { // portal-only: row removed
			t.Fatalf("delete %s: %d %s", it.kind, w.Code, w.Body.String())
		}
	}
	if n, _ := store.ConfigItems(ce.st.DB); len(n) != 0 {
		t.Fatalf("overlay not empty after deleting portal-only items: %+v", n)
	}
	if got := ce.applied.Load(); got != 15 {
		t.Fatalf("Apply called %d times, want 15", got)
	}

	// A file item: delete tombstones it, reset brings it back.
	if w := ce.act("rules", "r1", "delete"); w.Code != 303 || len(ce.rules()) != 0 {
		t.Fatalf("tombstone: %d %v", w.Code, ce.rules())
	}
	if w := ce.get("/config/rules"); !strings.Contains(w.Body.String(), "deleted") {
		t.Fatal("tombstone not listed")
	}
	if w := ce.act("rules", "r1", "reset"); w.Code != 303 || len(ce.rules()) != 1 {
		t.Fatalf("reset: %d %v", w.Code, ce.rules())
	}
	// An override of a file item shows as such, and a new rule reaches s.Config().
	ce.saveYAML("rules", "r1", `{source: gh, when: "false", action: {cmd: [echo, edited]}}`)
	if r := ce.cur.Load().Rules[0]; r.When != "false" {
		t.Fatalf("override not applied: %+v", r)
	}
	if w := ce.get("/config/rules"); !strings.Contains(w.Body.String(), "override") {
		t.Fatal("no override badge")
	}
}

func TestConfigFormMapping(t *testing.T) {
	ce := newCfgEnv(t)
	w := ce.post("/config/rules/new/save", url.Values{"mode": {"form"}, "name": {"fm"}, "rev": {"0"},
		"f.source": {"gh"}, "f.when": {"true"}, "f.on": {"each"}, "f.id": {"event.n"}, "f.approve": {"on"},
		"f.action.cmd": {"echo\nhello {{.event.n}}\n"}})
	if w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got *config.Rule
	for i, r := range ce.cur.Load().Rules {
		if r.Name == "fm" {
			got = &ce.cur.Load().Rules[i]
		}
	}
	if got == nil || got.On != "each" || !got.Approve || len(got.Action.Cmd) != 2 || got.Action.Cmd[1] != "hello {{.event.n}}" {
		t.Fatalf("rule: %+v", got)
	}
}

func TestConfigInvalidStoresNothing(t *testing.T) {
	ce := newCfgEnv(t)
	w := ce.saveYAML("rules", "bad", `{source: nope, when: "true", action: {cmd: [x]}}`)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "unknown source") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if n, _ := store.ConfigItems(ce.st.DB); len(n) != 0 || ce.latest() != 0 || ce.applied.Load() != 0 {
		t.Fatal("invalid item left state behind")
	}
	if w := ce.saveYAML("rules", "bad", `{{{`); w.Code != 422 {
		t.Fatalf("garbage yaml: %d", w.Code)
	}
	if w := ce.saveYAML("rules", "bad", `{name: other, source: gh, when: "true", action: {cmd: [x]}}`); w.Code != 422 {
		t.Fatalf("name mismatch: %d", w.Code)
	}
	if w := ce.post("/config/rules/new/save", url.Values{"mode": {"yaml"}, "name": {"../etc"}, "yaml": {"{}"}, "rev": {"0"}}); w.Code != 422 {
		t.Fatalf("bad name: %d", w.Code)
	}
}

func TestConfigStaleRevision(t *testing.T) {
	ce := newCfgEnv(t)
	ce.saveYAML("rules", "r2", `{source: gh, when: "true", action: {cmd: [a]}}`) // revision 1
	w := ce.post("/config/rules/r3/save", url.Values{"mode": {"yaml"}, "name": {"r3"}, "rev": {"0"},
		"yaml": {`{source: gh, when: "true", action: {cmd: [b]}}`}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "changed since you opened it") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(ce.rules()) != 2 {
		t.Fatalf("stale save applied: %v", ce.rules())
	}
}

func TestConfigCheckDoesNotWrite(t *testing.T) {
	ce := newCfgEnv(t)
	w := ce.post("/config/rules/r2/check", url.Values{"mode": {"yaml"}, "name": {"r2"}, "rev": {"0"},
		"yaml": {`{source: gh, when: "true", action: {cmd: [echo, two]}}`}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "+") || !strings.Contains(w.Body.String(), "r2") || !strings.Contains(w.Body.String(), "Confirm and save") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if n, _ := store.ConfigItems(ce.st.DB); len(n) != 0 || ce.latest() != 0 {
		t.Fatal("check wrote")
	}
}

func TestConfigRestoreAndExport(t *testing.T) {
	ce := newCfgEnv(t)
	ce.saveYAML("rules", "r2", `{source: gh, when: "true", action: {cmd: [two]}}`) // rev 1
	ce.act("rules", "r2", "delete")                                                // rev 2: gone
	if len(ce.rules()) != 1 {
		t.Fatalf("%v", ce.rules())
	}
	if w := ce.post("/history/1/restore", url.Values{"rev": {strconv.FormatInt(ce.latest(), 10)}}); w.Code != 303 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if len(ce.rules()) != 2 || ce.latest() != 3 {
		t.Fatalf("restore did not make a new revision: %v rev %d", ce.rules(), ce.latest())
	}
	w := ce.get("/history/export")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "r2") || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{"/history", "/history/1", "/config/rules", "/config/rules/new"} {
		if w := ce.get(p); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := ce.get("/history/99"); w.Code != 404 {
		t.Fatalf("missing revision: %d", w.Code)
	}
}

func TestConfigSecretNeverLeaks(t *testing.T) {
	ce := newCfgEnv(t)
	const secret = "SUPERSECRET123"
	form := url.Values{"mode": {"form"}, "name": {"s3"}, "rev": {"0"}, "f.type": {"webhook"}, "f.signature": {"github"}, "f.secret": {secret}}
	if w := ce.post("/config/sources/new/check", form); w.Code != 200 || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("check: %d leaked=%v", w.Code, strings.Contains(w.Body.String(), secret))
	}
	if w := ce.post("/config/sources/new/save", form); w.Code != 303 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	p := filepath.Join(ce.dir, "secrets", "sources-s3-secret")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", st, err)
	}
	if d, _ := os.Stat(filepath.Dir(p)); d.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode %v", d.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != secret {
		t.Fatal("secret not stored")
	}
	if got := ce.cur.Load().Sources["s3"].Secret.Value; got != secret {
		t.Fatalf("config did not resolve the stored secret: %q", got)
	}
	// An empty secret field on a later save keeps the stored ref.
	again := url.Values{"mode": {"form"}, "name": {"s3"}, "rev": {strconv.FormatInt(ce.latest(), 10)}, "f.type": {"webhook"}, "f.signature": {"sha256"}, "f.signature_header": {"X-S"}}
	if w := ce.post("/config/sources/s3/save", again); w.Code != 303 || ce.cur.Load().Sources["s3"].Secret.Value != secret {
		t.Fatalf("empty secret did not keep the ref: %d", w.Code)
	}

	var all []string
	for _, pth := range []string{"/config/sources", "/config/sources/s3", "/history", "/history/1", "/history/2", "/history/export", "/audit", "/sources", "/logins"} {
		all = append(all, ce.get(pth).Body.String())
	}
	all = append(all, ce.api("GET", "/api/config/sources/s3", "").Body.String(), ce.api("GET", "/api/config/export", "").Body.String(),
		ce.api("GET", "/api/config/history/1", "").Body.String(), ce.api("GET", "/api/audit", "").Body.String())
	revs, _ := store.Revisions(ce.st.DB, 10)
	for _, r := range revs {
		all = append(all, r.ItemsJSON, r.Diff, r.Summary)
	}
	rows, _ := store.ListAudit(ce.st.DB, 100)
	for _, a := range rows {
		all = append(all, a.Detail, a.Event)
	}
	for i, b := range all {
		if strings.Contains(b, secret) {
			t.Fatalf("secret leaked in output %d: %.200s", i, b)
		}
	}
	if !strings.Contains(ce.get("/config/sources/s3").Body.String(), "•••• set (stored)") {
		t.Fatal("secret field does not say it is set")
	}
}

func TestConfigRefusedKinds(t *testing.T) {
	ce := newCfgEnv(t)
	for _, k := range []string{"server", "limits", "units", "bogus"} {
		if w := ce.get("/config/" + k); w.Code != 404 {
			t.Errorf("GET /config/%s: %d", k, w.Code)
		}
		if w := ce.saveYAML(k, "x", "{}"); w.Code != 404 {
			t.Errorf("POST /config/%s/x/save: %d", k, w.Code)
		}
		if w := ce.api("PUT", "/api/config/"+k+"/x", `{"yaml":"a: 1"}`); w.Code != 400 {
			t.Errorf("PUT /api/config/%s/x: %d", k, w.Code)
		}
		if w := ce.api("GET", "/api/config/"+k, ""); w.Code != 400 {
			t.Errorf("GET /api/config/%s: %d", k, w.Code)
		}
	}
	if ce.latest() != 0 {
		t.Fatal("a refused kind created a revision")
	}
}

func TestConfigNeedsSessionAndCSRF(t *testing.T) {
	ce := newCfgEnv(t)
	rev := url.Values{"rev": {"0"}, "mode": {"yaml"}, "yaml": {"{}"}, "name": {"x"}}
	for _, p := range []string{"/config/rules/x/save", "/config/rules/x/check", "/config/rules/r1/delete", "/config/rules/r1/reset",
		"/history/1/restore", "/logins", "/logins/x/delete"} {
		bad := url.Values{"csrf": {"nope"}}
		for k, v := range rev {
			bad[k] = v
		}
		if w := ce.do("POST", p, bad, func(r *http.Request) { r.AddCookie(ce.c) }); w.Code != 403 {
			t.Errorf("%s with a bad csrf: %d", p, w.Code)
		}
		if w := ce.do("POST", p, rev, nil); w.Code != 401 {
			t.Errorf("%s without a session: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/config/rules", "/history", "/history/export"} {
		if w := ce.do("GET", p, nil, nil); w.Code != 303 {
			t.Errorf("GET %s without a session: %d", p, w.Code)
		}
	}
	if w := ce.do("GET", "/api/config/rules", nil, nil); w.Code != 401 {
		t.Fatalf("api without a token: %d", w.Code)
	}
	if ce.latest() != 0 {
		t.Fatal("unauthenticated request changed config")
	}
}

func TestConfigAPI(t *testing.T) {
	ce := newCfgEnv(t)
	if w := ce.api("GET", "/api/config/rules", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"r1"`) || !strings.Contains(w.Body.String(), `"file"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("PUT", "/api/config/rules/api1", `{"yaml":"source: gh\nwhen: \"true\"\naction: {cmd: [echo]}\n","rev":0}`); w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.Join(ce.rules(), ","), "api1") {
		t.Fatalf("not applied: %v", ce.rules())
	}
	if w := ce.api("PUT", "/api/config/rules/api2", `{"yaml":"source: gh\nwhen: \"true\"\naction: {cmd: [echo]}\n","rev":0}`); w.Code != 409 {
		t.Fatalf("stale put: %d", w.Code)
	}
	if w := ce.api("PUT", "/api/config/rules/bad", `{"yaml":"source: nope\nwhen: \"true\"\naction: {cmd: [x]}\n"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "unknown source") {
		t.Fatalf("invalid put: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("PUT", "/api/config/sources/inl", `{"yaml":"type: webhook\nsecret: inline-value\nsignature: github\n"}`); w.Code != 400 {
		t.Fatalf("inline secret accepted by the API: %d", w.Code)
	}
	if w := ce.api("GET", "/api/config/rules/api1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"portal"`) {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("GET", "/api/config/history", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "rules/api1 created") {
		t.Fatalf("history: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("DELETE", "/api/config/rules/api1", ""); w.Code != 200 || strings.Contains(strings.Join(ce.rules(), ","), "api1") {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := ce.api("GET", "/api/config/rules/api1", ""); w.Code != 404 {
		t.Fatalf("get deleted: %d", w.Code)
	}
}

func TestLoginsAddDelete(t *testing.T) {
	ce := newCfgEnv(t)
	const key = "sk-LEAKY-API-KEY"
	if w := ce.post("/logins", url.Values{"name": {"k1"}, "provider": {"claude"}, "kind": {"apikey"}, "value": {key}}); w.Code != 303 {
		t.Fatalf("apikey: %d %s", w.Code, w.Body.String())
	}
	if c := ce.cur.Load().Credentials["k1"]; c == nil || c.APIKey.Value != key {
		t.Fatalf("api key credential not live: %+v", c)
	}
	if w := ce.post("/logins", url.Values{"name": {"t1"}, "provider": {"claude"}, "kind": {"token"}, "value": {"setup-token-value"}}); w.Code != 303 {
		t.Fatalf("token: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(ce.dir, "credentials", "t1", "oauth-token")); err != nil {
		t.Fatalf("token not stored: %v", err)
	}
	if w := ce.post("/logins", url.Values{"name": {"bad"}, "provider": {"claude"}, "kind": {"login"}, "value": {"not json"}}); w.Code != 422 {
		t.Fatalf("bad login file: %d", w.Code)
	}
	if ce.cur.Load().Credentials["bad"] != nil {
		t.Fatal("an invalid login created a credential")
	}
	page := ce.get("/connections").Body.String()
	if !strings.Contains(page, "t1") || strings.Contains(page, key) || strings.Contains(page, "setup-token-value") {
		t.Fatal("logins page wrong or leaking")
	}
	if w := ce.post("/logins/t1/delete", nil); w.Code != 303 || ce.cur.Load().Credentials["t1"] != nil {
		t.Fatalf("delete: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(ce.dir, "credentials", "t1")); !os.IsNotExist(err) {
		t.Fatal("login files remain")
	}
	revs, _ := store.Revisions(ce.st.DB, 10)
	for _, r := range revs {
		if strings.Contains(r.ItemsJSON+r.Diff, key) {
			t.Fatal("api key in a revision")
		}
	}
}
