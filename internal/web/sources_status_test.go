package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func sourcesPage(t *testing.T, e *env) string {
	c, _ := e.login()
	return e.do("GET", "/sources", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
}

func sourceViews(t *testing.T, e *env) []sourceView {
	srcs, err := e.srv.sources()
	must(t, err)
	return srcs
}

func statusEnv(t *testing.T) *env {
	return newEnv(t, func(o *Options) {
		o.Cfg.Sources["tools"] = &config.Source{Type: "mcp", Connection: "sn"}
		o.Cfg.Sources["foo-docs"] = &config.Source{Type: "mcp"}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSourcesStatusWebhook(t *testing.T) {
	e := statusEnv(t)
	for _, v := range sourceViews(t, e) {
		if v.Name == "gh" && v.Health != "idle" {
			t.Errorf("no delivery: health = %s", v.Health)
		}
	}
	// a refusal older than the delivery stays hidden
	must(t, store.SetSourceReject(e.st.DB, "gh", time.Now().Add(-time.Hour), 401, "old refusal"))
	if p := sourcesPage(t, e); !strings.Contains(p, "Waiting for the first delivery.") {
		t.Errorf("no delivery yet: %s", p)
	}
	must(t, store.SetSourceEvent(e.st.DB, "gh", map[string]any{"a": 1}, nil))
	p := sourcesPage(t, e)
	if !strings.Contains(p, "Last delivery") || strings.Contains(p, "Refused") || strings.Contains(p, "old refusal") {
		t.Errorf("delivered: %s", p)
	}
	for _, v := range sourceViews(t, e) {
		if v.Name == "gh" && v.Health != "ok" {
			t.Errorf("health = %s", v.Health)
		}
	}
	// a refusal newer than the delivery shows; health stays ok
	e.now = e.now.Add(time.Hour)
	must(t, store.SetSourceReject(e.st.DB, "gh", time.Now().Add(time.Hour), 401, "bad signature"))
	if p := sourcesPage(t, e); !strings.Contains(p, "Refused") || !strings.Contains(p, "bad signature (401)") {
		t.Errorf("reject line: %s", p)
	}
}

func TestSourcesStatusToolsOnly(t *testing.T) {
	e := statusEnv(t)
	health := func() sourceView {
		for _, v := range sourceViews(t, e) {
			if v.Name == "tools" {
				return v
			}
		}
		t.Fatal("no tools source")
		return sourceView{}
	}
	if v := health(); v.Health != "idle" || !strings.Contains(sourcesPage(t, e), "Agent tools · not tested yet") {
		t.Errorf("untested: %+v", v)
	}
	must(t, store.SetConnectionCheck(e.st.DB, store.ConnectionCheck{Connection: "sn", At: e.now, OK: true, Detail: "siphon.integration"}))
	if v := health(); v.Health != "ok" || !strings.Contains(sourcesPage(t, e), "connection test ok") || !strings.Contains(sourcesPage(t, e), "siphon.integration") {
		t.Errorf("ok check: %+v", v)
	}
	must(t, store.SetConnectionCheck(e.st.DB, store.ConnectionCheck{Connection: "sn", At: e.now, OK: false, Detail: "401"}))
	if v := health(); v.Health != "error" || v.LastError != "Connection test failed: 401" || !strings.Contains(sourcesPage(t, e), "Connection test failed: 401") {
		t.Errorf("failed check: %+v", v)
	}
	body := e.do("GET", "/api/sources", nil, bearer).Body.String()
	if !strings.Contains(body, `"check"`) || !strings.Contains(body, `"tools_only":true`) {
		t.Errorf("api: %s", body)
	}
}

func TestSourcesStatusLegacyKeyAndAPI(t *testing.T) {
	e := statusEnv(t)
	must(t, store.SetConnectionCheck(e.st.DB, store.ConnectionCheck{Connection: "foo", At: e.now, OK: true, Detail: "legacy"}))
	for _, v := range sourceViews(t, e) {
		if v.Name == "foo-docs" && (v.Check == nil || v.Health != "ok") {
			t.Errorf("legacy key: %+v", v)
		}
	}
	must(t, store.SetSourceEvent(e.st.DB, "gh", map[string]any{"a": 1}, nil))
	if body := e.do("GET", "/api/sources", nil, bearer).Body.String(); !strings.Contains(body, `"last_event_at"`) {
		t.Errorf("api: %s", body)
	}
}

func TestSourcesStatusPolledUnchanged(t *testing.T) {
	e := statusEnv(t)
	if p := sourcesPage(t, e); !strings.Contains(p, "Not polled yet.") {
		t.Errorf("unpolled: %s", p)
	}
	must(t, store.PutSourceState(e.st.DB, "disk", e.now, ""))
	if p := sourcesPage(t, e); !strings.Contains(p, "Last poll") {
		t.Errorf("polled: %s", p)
	}
}

func TestSourcesStatusUnpolledHidesStaleError(t *testing.T) {
	r := config.Rule{Name: "r", Source: "tools", When: "true", Action: config.Action{Cmd: []string{"true"}}}
	e := newEnv(t, func(o *Options) {
		o.Cfg.Sources["tools"] = &config.Source{Type: "mcp", Connection: "sn"}
		o.Cfg.Rules = []config.Rule{r}
	})
	must(t, store.PutSourceState(e.st.DB, "tools", e.now, "stale tools failure"))
	must(t, store.PutSourceState(e.st.DB, "disk", e.now, "disk is failing"))
	p := sourcesPage(t, e)
	if strings.Contains(p, "stale tools failure") || !strings.Contains(p, "disk is failing") {
		t.Errorf("page: %s", p)
	}
	if _, c := e.explain(t, "r"); hasCode(c, "source_error") {
		t.Errorf("source_error for an unpolled source: %v", c)
	}
}
