package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func jbody(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestConnAPIServices(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/user" && r.Header.Get("PRIVATE-TOKEN") == "glpat-SECRET2" {
			w.Write([]byte(`{"username":"olaf"}`))
			return
		}
		http.Error(w, "nope", 401)
	}))
	defer gl.Close()
	u, _ := url.Parse(gl.URL)
	ce := newCfgEnvFile(t, strings.Replace(awsWebCfg, "server: { sandbox: none, db: DIR/s.db,",
		`server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["`+u.Host+`"] },`, 1))
	ce.testAWS = func(context.Context, string) (string, time.Time, []string, error) {
		return "arn:aws:sts::1:assumed-role/x", time.Time{}, []string{"a", "b"}, nil
	}

	w := ce.apiAs("POST", "/api/services/github", jbody(map[string]any{"name": "ghub", "token": "ghp_SECRET1", "mode": "remote", "webhook": true}), "ci")
	var done serviceDone
	json.Unmarshal(w.Body.Bytes(), &done)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || len(done.HookSecret) != 64 || done.HookURL != "https://siphon.example/hook/ghub-hooks" || done.Name != "ghub" {
		t.Fatalf("github: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ghp_SECRET1") {
		t.Fatal("token echoed")
	}
	if c := ce.cur.Load().Sources["ghub"]; c == nil || c.Auth.Bearer.Value != "ghp_SECRET1" {
		t.Fatal("not applied")
	}
	if revs, _ := storeRevs(ce); revs != "api:ci" {
		t.Fatalf("actor %q", revs)
	}
	if w := ce.api("POST", "/api/services/github", jbody(map[string]any{"name": "ghub", "token": "x"})); w.Code != 422 || !strings.Contains(w.Body.String(), `"errors"`) {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("POST", "/api/services/github", jbody(map[string]any{"name": "g2"})); w.Code != 422 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := ce.api("POST", "/api/services/github", `{`); w.Code != 400 {
		t.Fatalf("bad body: %d", w.Code)
	}

	w = ce.api("POST", "/api/services/gitlab", jbody(map[string]any{"name": "gl", "base": gl.URL, "project": "grp/proj", "token": "glpat-SECRET2", "webhook": true}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hook_secret") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("gitlab: %d %s", w.Code, w.Body.String())
	}
	w = ce.api("POST", "/api/services/gl/test", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"olaf"`) || strings.Contains(w.Body.String(), "glpat") {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("POST", "/api/services/nope/test", ""); w.Code != 404 {
		t.Fatalf("test unknown: %d", w.Code)
	}

	w = ce.api("POST", "/api/services/aws", jbody(map[string]any{"name": "aws", "region": "eu-west-1", "mode": "role",
		"role_arn": "arn:aws:iam::123456789012:role/ro", "access_key_id": "AKIAFAKEBASE", "secret_access_key": "fakeBaseSecret", "servers": []string{"cloudwatch"}, "webhook": true}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hook_header":"X-Siphon-Key"`) || strings.Contains(w.Body.String(), "fakeBaseSecret") {
		t.Fatalf("aws: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("POST", "/api/services/aws-cloudwatch/test", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"tools":2`) {
		t.Fatalf("aws test: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("POST", "/api/services/aws", jbody(map[string]any{"name": "bad", "region": "x", "servers": []string{}})); w.Code != 422 {
		t.Fatalf("aws no servers: %d", w.Code)
	}

	// Nothing secret in any GET afterwards.
	all := ""
	for _, p := range []string{"/api/connections", "/api/config/sources/ghub", "/api/config/sources/ghub-hooks", "/api/config/credentials/aws", "/api/config/export", "/api/config/history/1", "/api/audit"} {
		r := ce.api("GET", p, "")
		if strings.Contains(r.Body.String(), done.HookSecret) {
			t.Fatalf("one-time secret visible on %s", p)
		}
		all += r.Body.String()
	}
	for _, s := range []string{"ghp_SECRET1", "glpat-SECRET2", "fakeBaseSecret", "AKIAFAKEBASE"} {
		if strings.Contains(all, s) {
			t.Fatalf("%s visible in a GET", s)
		}
	}
}

func TestConnAPIModelsAndLogins(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer sk-secret-123" {
			w.Write([]byte(`{"data":[{"id":"gpt-oss-20b"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer stub.Close()
	u, _ := url.Parse(stub.URL)
	ce := newCfgEnvFile(t, strings.Replace(cfgFile, "server: { sandbox: none, db: DIR/s.db }",
		`server: { sandbox: none, db: DIR/s.db, models: { private_endpoints: ["`+u.Host+`"] } }`, 1))

	if w := ce.api("POST", "/api/connections/models", jbody(map[string]any{"name": "oai", "preset": "openai", "url": stub.URL + "/v1", "api_key": "sk-secret-123"})); w.Code != 200 || strings.Contains(w.Body.String(), "sk-secret") {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	w := ce.api("POST", "/api/connections/models/oai/test", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "gpt-oss-20b") || strings.Contains(w.Body.String(), "sk-secret") {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("POST", "/api/connections/models/nope/test", ""); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	if w := ce.api("POST", "/api/connections/models", jbody(map[string]any{"name": "x", "preset": "nope"})); w.Code != 422 {
		t.Fatalf("bad preset: %d", w.Code)
	}
	if w := ce.api("POST", "/api/connections/models", jbody(map[string]any{"name": "pv", "preset": "ollama", "url": "http://127.0.0.1:9"})); w.Code != 422 || !strings.Contains(w.Body.String(), "private_endpoints") {
		t.Fatalf("private: %d %s", w.Code, w.Body.String())
	}

	if w := ce.api("POST", "/api/connections/logins", jbody(map[string]any{"name": "k1", "provider": "claude", "kind": "apikey", "value": "sk-LEAKY-1"})); w.Code != 200 || strings.Contains(w.Body.String(), "LEAKY") {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("POST", "/api/connections/logins", jbody(map[string]any{"name": "bad", "provider": "claude", "kind": "login", "value": "not json"})); w.Code != 422 {
		t.Fatalf("bad login: %d", w.Code)
	}
	if w := ce.api("POST", "/api/connections/logins", jbody(map[string]any{"name": "x", "provider": "claude", "kind": "weird", "value": "v"})); w.Code != 422 {
		t.Fatalf("bad kind: %d", w.Code)
	}
	w = ce.api("GET", "/api/connections", "")
	var got struct {
		Logins []struct{ Name, Provider, Status string }
		Models []struct{ Name, Provider, URL string }
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || len(got.Logins) != 1 || got.Logins[0].Name != "k1" || got.Logins[0].Status != "apikey" || len(got.Models) != 1 || got.Models[0].URL != stub.URL+"/v1" ||
		strings.Contains(w.Body.String(), "LEAKY") || strings.Contains(w.Body.String(), "sk-secret") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("DELETE", "/api/connections/logins/k1", ""); w.Code != 200 || ce.cur.Load().Credentials["k1"] != nil {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := ce.api("DELETE", "/api/connections/logins/k1", ""); w.Code != 404 {
		t.Fatalf("delete again: %d", w.Code)
	}
	if w := ce.do("GET", "/api/connections", nil, nil); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
}
