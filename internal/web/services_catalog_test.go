package web

import (
	"encoding/json"
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
	ce := newCfgEnvFile(t, strings.Replace(awsWebCfg, "mcp_packages: {", "mcp_packages: {\n  github: {command: [gh], hosts: [api.github.com]},", 1))
	for _, e := range catalog.All() {
		if e.Status != "available" {
			continue
		}
		v := map[string]string{"name": "t-" + e.ID, "project": "g/p", "profile": "p", "webhook": "on"}
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
			case "choice":
				v[f.Key] = f.Choices[0]
			default:
				if f.Default == "" {
					v[f.Key] = "dummy"
				}
			}
		}
		if e.ID == "aws" { // role mode needs a pair of keys; profile mode none
			delete(v, "access_key_id")
			delete(v, "secret_access_key")
		}
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
			t.Errorf("%s: commit: %d %s", e.ID, w.Code, w.Body.String())
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
