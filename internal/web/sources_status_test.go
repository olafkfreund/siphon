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

func statusEnv(t *testing.T) *env {
	return newEnv(t, func(o *Options) {
		o.Cfg.Sources["tools"] = &config.Source{Type: "mcp", Connection: "sn"}
	})
}

func TestSourcesStatusWebhook(t *testing.T) {
	e := statusEnv(t)
	if p := sourcesPage(t, e); !strings.Contains(p, "Waiting for the first delivery.") {
		t.Errorf("no delivery yet: %s", p)
	}
	store.SetSourceEvent(e.st.DB, "gh", map[string]any{"a": 1}, nil)
	p := sourcesPage(t, e)
	if !strings.Contains(p, "Last delivery") || strings.Contains(p, "Refused") {
		t.Errorf("delivered: %s", p)
	}
	srcs, _ := e.srv.sources()
	for _, v := range srcs {
		if v.Name == "gh" && v.Health != "ok" {
			t.Errorf("health = %s", v.Health)
		}
	}
	// a refusal newer than the delivery shows; health stays ok
	e.now = e.now.Add(time.Hour)
	store.SetSourceReject(e.st.DB, "gh", time.Now().Add(time.Hour), 401, "bad signature")
	if p := sourcesPage(t, e); !strings.Contains(p, "Refused") || !strings.Contains(p, "bad signature (401)") {
		t.Errorf("reject line: %s", p)
	}
}

func TestSourcesStatusToolsOnly(t *testing.T) {
	e := statusEnv(t)
	health := func() sourceView {
		srcs, err := e.srv.sources()
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range srcs {
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
	store.SetConnectionCheck(e.st.DB, store.ConnectionCheck{Connection: "sn", At: e.now, OK: true, Detail: "siphon.integration"})
	if v := health(); v.Health != "ok" || !strings.Contains(sourcesPage(t, e), "connection test ok") || !strings.Contains(sourcesPage(t, e), "siphon.integration") {
		t.Errorf("ok check: %+v", v)
	}
	store.SetConnectionCheck(e.st.DB, store.ConnectionCheck{Connection: "sn", At: e.now, OK: false, Detail: "401"})
	if v := health(); v.Health != "error" || v.LastError != "Connection test failed: 401" {
		t.Errorf("failed check: %+v", v)
	}
}

func TestSourcesStatusPolledUnchanged(t *testing.T) {
	e := statusEnv(t)
	if p := sourcesPage(t, e); !strings.Contains(p, "Not polled yet.") {
		t.Errorf("unpolled: %s", p)
	}
	store.PutSourceState(e.st.DB, "disk", e.now, "")
	if p := sourcesPage(t, e); !strings.Contains(p, "Last poll") {
		t.Errorf("polled: %s", p)
	}
}
