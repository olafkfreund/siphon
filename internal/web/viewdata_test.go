package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// A webhook source is hot while its last delivery is recent, not once it is old.
func TestSourceHot(t *testing.T) {
	for ago, want := range map[time.Duration]bool{5 * time.Minute: true, 20 * time.Minute: false} {
		e := newEnv(t, nil)
		if _, err := e.st.DB.Exec(`INSERT INTO source_state(source,event_at) VALUES ('gh',?)`, e.now.Add(-ago).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		srcs, err := e.srv.sources()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range srcs {
			if s.Name == "gh" && s.Hot != want {
				t.Errorf("%v ago: hot=%v want %v", ago, s.Hot, want)
			}
			if s.Name == "disk" && s.Hot {
				t.Error("a never-polled source is hot")
			}
		}
	}
}

// The job page knows the rule's source, condition and mode, and an agent's provider.
func TestJobDetailLineage(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Credentials = map[string]*config.Credential{"g": {Provider: "openai", Preset: "groq"}}
		o.Cfg.Agents = map[string]*config.Agent{"mg": {Kind: "model", Credential: "g"}}
		o.Cfg.Rules = []config.Rule{{Name: "r1", Source: "gh", When: "x > 1", On: "each", Action: config.Action{Agent: "mg"}}}
	})
	tx, _ := e.st.DB.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "r1", State: "queued", ActionJSON: `{"action":{"Agent":"mg"}}`}, e.now)
	tx.Commit()
	v, ok, err := e.srv.jobDetail(id)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if v.Source != "gh" || v.SourceType != "webhook" || v.When != "x > 1" || v.On != "each" || v.Provider != "groq" {
		t.Fatalf("%+v", v)
	}
}

// runs() resolves a model agent's provider like the rules page does.
func TestRunsModelProvider(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Credentials = map[string]*config.Credential{"o": {Provider: "ollama"}}
		o.Cfg.Agents = map[string]*config.Agent{"mo": {Kind: "model", Credential: "o"}}
	})
	tx, _ := e.st.DB.Begin()
	store.InsertJob(tx, store.Job{Rule: "r", State: "queued", ActionJSON: `{"action":{"Agent":"mo"}}`}, e.now)
	tx.Commit()
	js, err := e.srv.runs("", 10)
	if err != nil || len(js) != 1 || js[0].Provider != "ollama" {
		t.Fatalf("%v %+v", err, js)
	}
}

func (e *env) page(path string) string {
	c, _ := e.login()
	return e.do("GET", path, nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
}

// The Dashboard reads a webhook's deliveries from its event time (it used to say
// "waiting for a delivery" forever), and shows a schedule's next run.
func TestDashboardSourceStatus(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Sources["tick"] = &config.Source{Type: "schedule", At: "0 6 * * *", Timezone: "UTC"}
	})
	b := e.page("/")
	if !strings.Contains(b, "waiting for a delivery") || !strings.Contains(b, "next run") {
		t.Error("before a delivery: want 'waiting for a delivery' and 'next run'")
	}
	if _, err := e.st.DB.Exec(`INSERT INTO source_state(source,event_at) VALUES ('gh',?)`, e.now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if b = e.page("/"); !strings.Contains(b, "last delivery") || strings.Contains(b, "waiting for a delivery") {
		t.Error("after a delivery: want 'last delivery'")
	}
}

// Approve and deny stay plain POST forms on the Dashboard.
func TestDashboardApprovalForms(t *testing.T) {
	e := newEnv(t, nil)
	id := e.pendingJob()
	b := e.page("/")
	for _, want := range []string{`action="/approvals/` + itoa(int(id)) + `/approve"`, `action="/approvals/` + itoa(int(id)) + `/deny"`, "item-card need is-wait"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Jobs: no emoji glyphs anywhere, a failed row is flagged, the filter bar is there and hidden.
func TestJobsPageMarkup(t *testing.T) {
	e := newEnv(t, nil)
	tx, _ := e.st.DB.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "disk-full", State: "running", ActionJSON: `{"action":{"Unit":"nix-gc.service"}}`}, e.now)
	tx.Commit()
	store.FinishJob(e.st.DB, id, "failed", 2, "boom", e.now)
	b := e.page("/jobs")
	if strings.ContainsAny(b, "✋⟳◷✓✕⊘") {
		t.Error("emoji glyph in the markup")
	}
	for _, want := range []string{"is-err", `data-state="failed"`, `data-kind="unit"`, `class="filter-bar" data-filter-scope="jobs" hidden`, "data-filter-item"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
	if b = e.page("/jobs?state=done"); strings.Contains(b, "filter-bar") {
		t.Error("filter bar on an empty list")
	}
}

// The job page opens with the source -> rule -> action chain.
func TestJobPageHero(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Rules = []config.Rule{{Name: "gh-push", Source: "gh", When: "true", On: "each", Action: config.Action{Cmd: []string{"deploy"}}}}
	})
	tx, _ := e.st.DB.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "gh-push", State: "queued", ActionJSON: `{"action":{"Cmd":["deploy"]},"env":{"event":{"a":1}}}`}, e.now)
	tx.Commit()
	b := e.page("/jobs/" + itoa(int(id)))
	for _, want := range []string{"item-card hero", `title="gh"`, `href="/config/rules/gh-push"`, "each event", "<b>deploy</b>", `data-target="job-event"`} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
}
