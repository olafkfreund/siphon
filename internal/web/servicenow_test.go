package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServiceNowConnectionTest(t *testing.T) {
	var deny atomic.Bool
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth_token.do":
			io.WriteString(w, `{"access_token":"sekrit-token","expires_in":1800}`)
		case deny.Load():
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"denied"}}`)
		case r.URL.Path == "/api/now/table/sys_user":
			io.WriteString(w, `{"result":[{"user_name":"svc.siphon"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	u, _ := url.Parse(fake.URL)
	// The fake is on loopback, so it must be listed: the guard stays as it is.
	ce := newCfgEnvFile(t, strings.Replace(awsWebCfg, "server: { sandbox: none, db: DIR/s.db,",
		`server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["`+u.Host+`"] },`, 1))

	w := ce.api("POST", "/api/services/servicenow", jbody(map[string]any{"name": "snow", "instance": fake.URL,
		"client_id": "sekrit-id", "client_secret": "sekrit-secret"}))
	if w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	w = ce.api("POST", "/api/connections/snow/test", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"svc.siphon"`) || !strings.Contains(w.Body.String(), `"detail":"svc.siphon"`) || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}

	deny.Store(true)
	w = ce.api("POST", "/api/connections/snow/test", "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "401") || !strings.Contains(body, "denied") {
		t.Fatalf("denied: %d %s", w.Code, body)
	}
	for _, s := range []string{"sekrit-id", "sekrit-secret", "sekrit-token"} {
		if strings.Contains(body, s) {
			t.Fatalf("%s in %s", s, body)
		}
	}
}

const snRuleCfg = `server: { sandbox: none, db: DIR/s.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
  sn: { type: mcp, package: servicenow, read: { tool: t }, env: { SERVICENOW_INSTANCE_URL: env:AGW_HOOK, SERVICENOW_API_KEY: env:AGW_HOOK } }
rules:
  - name: note
    source: gh
    when: "true"
    cooldown: 1m
    action: { servicenow: { connection: sn, op: add_work_note, sys_id: "{{.event.id}}", fields: { text: "hi {{.event.n}}" } } }
`

func TestServiceNowRulePortal(t *testing.T) {
	ce := newCfgEnvFile(t, snRuleCfg)

	if w := ce.get("/rules"); !strings.Contains(w.Body.String(), "servicenow") || !strings.Contains(w.Body.String(), "add_work_note") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}

	// The form is replaced by a note; the YAML editor stays.
	w := ce.get("/config/rules/note")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "edit it as YAML") || strings.Contains(body, `name="mode" value="form"`) {
		t.Fatalf("edit: %d %s", w.Code, body)
	}

	// A form save is refused and nothing changes.
	w = ce.post("/config/rules/note/save", url.Values{"mode": {"form"}, "name": {"note"}, "rev": {"0"},
		"f.source": {"gh"}, "f.when": {"true"}, "f.action.cmd": {"echo"}})
	if w.Code != 422 || !strings.Contains(w.Body.String(), "edit it as YAML") {
		t.Fatalf("form save: %d %s", w.Code, w.Body.String())
	}
	if ce.latest() != 0 || ce.cur.Load().Rules[0].Action.ServiceNow == nil {
		t.Fatal("refused save changed the config")
	}

	// The test fire shows the rendered call.
	cfg := ce.cur.Load()
	tv := ruleTest(context.Background(), cfg, cfg.Rules[0], map[string]any{"id": "abc", "n": 7}, nil)
	if len(tv.Fires) != 1 || tv.Fires[0].Kind != "servicenow" || strings.Join(tv.Fires[0].Argv, "|") != "abc|text=hi 7" {
		t.Fatalf("%+v", tv.Fires)
	}
}
