package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/store"
)

// #135 A3: deleting an API-key login removes its key file.
func TestRemoveLoginDeletesAPIKeyFile(t *testing.T) {
	ce := newCfgEnv(t)
	if w := ce.post("/logins", url.Values{"name": {"k1"}, "provider": {"claude"}, "kind": {"apikey"}, "value": {"sk-x"}}); w.Code != 303 {
		t.Fatalf("add: %d", w.Code)
	}
	p := filepath.Join(ce.dir, "secrets", "credentials--k1+api_key")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if w := ce.post("/logins/k1/delete", nil); w.Code != 303 {
		t.Fatalf("delete: %d", w.Code)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("key file remains: %v", err)
	}
}

// #135 A2: Review with a pasted secret can't offer a Confirm that would lose it.
func TestReviewWithPendingSecretHasNoConfirm(t *testing.T) {
	ce := newCfgEnv(t)
	form := url.Values{"mode": {"form"}, "name": {"s5"}, "rev": {"0"}, "f.type": {"webhook"}, "f.signature": {"github"}, "f.secret": {"SUPERSECRET789"}}
	w := ce.post("/config/sources/new/check", form)
	b := w.Body.String()
	if w.Code != 200 || strings.Contains(b, `name="yaml"`) && strings.Contains(b, "Confirm and save") || !strings.Contains(b, "Secrets are not kept by Review") {
		t.Fatalf("%d %s", w.Code, b)
	}
}

// #135 A11: /check applies the same private-endpoint rule as save.
func TestCheckRejectsPrivateModelEndpoint(t *testing.T) {
	ce := newCfgEnv(t)
	srv := httptest.NewServer(http.NotFoundHandler()) // 127.0.0.1, not in models.private_endpoints
	defer srv.Close()
	form := url.Values{"mode": {"form"}, "name": {"fm"}, "f.provider": {"openai"}, "f.url": {srv.URL + "/v1"}, "f.concurrency": {"1"}, "rev": {"0"}}
	if w := ce.post("/config/credentials/new/check", form); w.Code != 422 {
		t.Fatalf("check: %d", w.Code)
	}
	if w := ce.post("/config/credentials/new/validate", form); !strings.Contains(w.Body.String(), "yaml-status err") {
		t.Fatalf("validate: %s", w.Body.String())
	}
}

// #135 A15: a ServiceNow step is labelled as such, not as cmd.
func TestServiceNowStepKind(t *testing.T) {
	ce := newCfgEnvFile(t, snRuleCfg)
	if w := ce.saveYAML("routines", "sn", `{steps: [{id: a, servicenow: {connection: sn, op: add_work_note, sys_id: x, fields: {text: hi}}}]}`); w.Code != 303 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if b := ce.get("/config/routines/sn").Body.String(); !strings.Contains(b, "servicenow <code>add_work_note incident</code>") {
		t.Fatalf("routine page: %s", b)
	}
	tx, _ := ce.st.DB.Begin()
	store.InsertJob(tx, store.Job{Rule: "r", State: "running", ActionJSON: `{"action":{"Routine":"sn"},"steps":[{"ID":"a","ServiceNow":{"Op":"add_work_note"}}]}`}, ce.now)
	tx.Commit()
	if b := html.UnescapeString(ce.get("/jobs/1").Body.String()); !strings.Contains(b, "servicenow add_work_note incident") {
		t.Fatalf("job page: %s", b)
	}
}
