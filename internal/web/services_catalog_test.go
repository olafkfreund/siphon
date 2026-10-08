package web

import (
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
