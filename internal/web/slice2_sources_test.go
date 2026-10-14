package web

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func TestSourcesCards(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Server.PublicURL = "https://siphon.example"
		o.Cfg.Sources["tick"] = &config.Source{Type: "schedule", At: "0 6 * * *", Timezone: "UTC"}
	})
	b := sourcesPage(t, e)
	for _, want := range []string{
		"item-card", `data-kind="webhook"`, `data-kind="schedule"`, `data-health="idle"`, `data-hot="false"`,
		"https://siphon.example/hook/gh", `data-action="copy" data-target="src-gh"`, "<code>0 6 * * *</code>",
		"2 rules", "every 1m0s", `data-filter-scope="sources"`, "/config/sources/new", `href="/config/sources/tick"`,
		"#i-timer",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
	// nothing empty: no empty paragraph, no emoji
	if regexp.MustCompile(`<p[^>]*>\s*</p>`).MatchString(b) || strings.ContainsAny(b, "✋⟳◷✓✕⊘") {
		t.Error("empty <p> or emoji")
	}
	// a viewer gets no New/Edit links
	c, _ := e.asRole(roleViewer, "v")
	vb := e.do("GET", "/sources", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if strings.Contains(vb, "/config/sources/") {
		t.Error("viewer sees admin links")
	}
}

func TestSourcesRefusingWebhookIsWait(t *testing.T) {
	e := newEnv(t, nil)
	must(t, store.SetSourceEvent(e.st.DB, "gh", map[string]any{"a": 1}, nil))
	e.now = e.now.Add(time.Hour)
	must(t, store.SetSourceReject(e.st.DB, "gh", e.now, 401, "bad signature"))
	b := sourcesPage(t, e)
	for _, want := range []string{"item-card is-wait", `data-health="stale"`, ">refusing<", "blocked is-wait"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestSourcesHotViaLastRun(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Sources["tick"] = &config.Source{Type: "schedule", At: "0 6 * * *", Timezone: "UTC"}
	})
	must(t, store.SetSourceEvent(e.st.DB, "tick", map[string]string{"scheduled_at": e.now.Add(-time.Minute).UTC().Format(time.RFC3339)}, nil))
	must(t, store.PutSourceState(e.st.DB, "tick", e.now.Add(-time.Hour), ""))
	if b := sourcesPage(t, e); !strings.Contains(b, "is-hot") || !strings.Contains(b, `data-hot="true"`) {
		t.Error("a recent schedule run is hot")
	}
}

// A key in a source URL's query string never reaches the page.
func TestDisplayURLDropsSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.vendor.com/v1/alerts?api_key=XYZ#frag": "https://api.vendor.com/v1/alerts",
		"https://u:p@host.example/path":                     "https://host.example/path",
		"http://127.0.0.1:9000/feed":                        "http://127.0.0.1:9000/feed",
	} {
		if got := displayURL(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}
