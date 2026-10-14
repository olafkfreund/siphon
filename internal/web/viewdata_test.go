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
		found := false
		for _, s := range srcs {
			found = found || s.Name == "gh"
			if s.Name == "gh" && s.Hot != want {
				t.Errorf("%v ago: hot=%v want %v", ago, s.Hot, want)
			}
			if s.Name == "disk" && s.Hot {
				t.Error("a never-polled source is hot")
			}
		}
		if !found {
			t.Fatal("no source gh")
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
		o.Cfg.Credentials = map[string]*config.Credential{"o": {Provider: "ollama"}, "g": {Provider: "openai", Preset: "groq"}}
		o.Cfg.Agents = map[string]*config.Agent{"mo": {Kind: "model", Credential: "o"}}
	})
	tx, err := e.st.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// a run from before snapshots: the live agent decides
	if _, err := store.InsertJob(tx, store.Job{Rule: "r", State: "queued", ActionJSON: `{"action":{"Agent":"mo"}}`}, e.now); err != nil {
		t.Fatal(err)
	}
	// a real run carries a snapshot (kind model): its credential decides, even though the agent is gone now
	if _, err := store.InsertJob(tx, store.Job{Rule: "r", State: "queued", ActionJSON: `{"action":{"Agent":"gone"},"agents":{"gone":{"Kind":"model","Credential":"g"}}}`}, e.now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	js, err := e.srv.runs("", 10)
	if err != nil || len(js) != 2 || js[0].Provider != "groq" || js[1].Provider != "ollama" { // newest first
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

// Slice 4: one regression test per verified bug, plus the new filter bars.
func (e *env) pageAs(r role, path string) string {
	c, _ := e.asRole(r, "u")
	return e.do("GET", path, nil, func(q *http.Request) { q.AddCookie(c) }).Body.String()
}

// Bugs 1 and the failed-channel flag, the deliveries filter, the chips-check.
func TestSlice4Notifications(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.ConfigPath = "x"
		o.Cfg.Notify = map[string]*config.Notify{"phone": {Type: "ntfy"}, "a.b": {Type: "ntfy"}}
	})
	if _, err := e.st.DB.Exec(`INSERT INTO notifications(channel,event,key,source,title,body,created_at,next_at,state,error) VALUES ('phone','failed','k','','t','b',?,?,'failed','boom')`, e.now.UnixMilli(), e.now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, r := range []role{roleViewer, roleOperator} {
		if b := e.pageAs(r, "/notifications"); strings.Contains(b, `id="add"`) || strings.Contains(b, `href="#add"`) {
			t.Errorf("%s sees the add form", r)
		}
	}
	b := e.pageAs(roleAdmin, "/notifications")
	if strings.Contains(b, "#ntest-") || strings.Count(b, `hx-target="next .conn-test"`) != 2 {
		t.Error("Test must target a relative .conn-test, not an id built from the name")
	}
	for _, want := range []string{`id="add"`, `href="#add"`, "chips-check", "item-card is-err", "stamp err", `data-filter-scope="deliveries" hidden`, `data-state="failed"`} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Bug 3 and the linked owners.
func TestSlice4Egress(t *testing.T) {
	off := false
	e := newEnv(t, func(o *Options) {
		o.Cfg.Agents = map[string]*config.Agent{"free": {Kind: "claude", Egress: config.EgressAgent{Enabled: &off}}, "tight": {Kind: "model"}}
	})
	b := e.page("/egress")
	for _, want := range []string{"stamp wait", "#i-shield", "Unrestricted", "No network", `href="/config/agents/free"`, `href="/config/agents/tight"`} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Bugs 4 and 6.
func TestSlice4Audit(t *testing.T) {
	e := newEnv(t, nil)
	if !strings.Contains(e.page("/audit"), `<div class="empty">No audit entries yet.</div></section>`) || strings.Contains(e.page("/audit"), "<table") {
		t.Error("empty state is not a full-width card")
	}
	tx, _ := e.st.DB.Begin()
	store.Audit(tx, e.now, "alice", "rule_added", 0, "r9")
	tx.Commit()
	b := e.page("/audit")
	if !strings.Contains(b, `<b class="c-actor">alice</b>`) || strings.Contains(b, "hide-sm") || !strings.Contains(b, `data-filter-scope="audit" hidden`) {
		t.Error("actor hidden or no filter bar")
	}
}

// Bugs 2 and 6 for History.
func TestSlice4History(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ConfigPath = "x" })
	if b := e.pageAs(roleAdmin, "/history"); !strings.Contains(b, "No portal changes yet.") || strings.Contains(b, "<table") {
		t.Error("history empty state")
	}
	tx, _ := e.st.DB.Begin()
	id, err := store.AddRevision(tx, e.now, "alice", "edit", "{}", "diff")
	tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	b := e.pageAs(roleAdmin, "/history/"+itoa(int(id)))
	if !strings.Contains(b, `data-confirm="Restore revision `+itoa(int(id))+`?`) {
		t.Error("Restore has no confirm")
	}
	if b = e.pageAs(roleAdmin, "/history"); !strings.Contains(b, "item-card row") || strings.Contains(b, "filter-bar") {
		t.Error("history rows, and no filter bar under 11 revisions")
	}
}

// Bug 5 and the template gallery filter.
func TestSlice4PillAndGallery(t *testing.T) {
	e := newEnv(t, nil)
	tx, _ := e.st.DB.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "r", State: "running", ActionJSON: `{"action":{"Unit":"u"}}`}, e.now)
	tx.Commit()
	store.FinishJob(e.st.DB, id, "cancelled", 0, "", e.now)
	if b := e.page("/jobs"); !strings.Contains(b, `pill cancelled"><svg class="ico" aria-hidden="true"><use href="#i-x"/></svg> Cancelled`) {
		t.Error("cancelled pill has no icon")
	}
	if b := e.page("/help/templates"); !strings.Contains(b, `data-filter-scope="templates" hidden`) || !strings.Contains(b, "data-filter-item") {
		t.Error("no gallery filter bar")
	}
}
