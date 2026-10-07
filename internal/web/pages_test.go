package web

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// Plan step 6: the job page shows what an agent is asked, redacts secrets in
// the triggering event, and lists the hosts the sandbox refused.
func TestJobDetailPage(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Agents = map[string]*config.Agent{"helper": {Kind: "claude", Prompt: "Answer: {{.event.q}}"}}
	})
	tx, _ := e.st.DB.Begin()
	id, err := store.InsertJob(tx, store.Job{Rule: "ask", State: "running",
		ActionJSON: `{"action":{"Agent":"helper"},"env":{"event":{"q":"What is 2+2?","api_key":"sk-SECRETVALUE"}},` +
			`"agents":{"helper":{"Kind":"claude","Prompt":"Answer: {{.event.q}}"}}}`}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	store.FinishJob(e.st.DB, id, "done", 0, "4\negress: blocked evil.example:443 (3)\n", e.now)
	c, _ := e.login()
	body := html.UnescapeString(e.do("GET", "/jobs/1", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String())
	for _, want := range []string{"Answer: What is 2+2?", "[redacted]", "evil.example:443", "3×"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "SECRETVALUE") {
		t.Error("event secret rendered")
	}
}

// Every new page renders behind login.
func TestStep6PagesRender(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.login()
	for _, p := range []string{"/", "/jobs", "/jobs?state=failed", "/approvals", "/rules", "/sources", "/audit", "/connections", "/egress"} {
		if w := e.do("GET", p, nil, func(r *http.Request) { r.AddCookie(c) }); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

// A job that failed for want of a login links straight to connecting it.
func TestJobFailureLinksToConnect(t *testing.T) {
	e := newEnv(t, nil)
	tx, _ := e.st.DB.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "ask", State: "running", ActionJSON: `{"action":{"Agent":"helper"}}`}, e.now)
	tx.Commit()
	store.FinishJob(e.st.DB, id, "failed", 1, "credential claude-max is not imported: run `siphon credentials import`", e.now)
	c, _ := e.login()
	body := e.do("GET", "/jobs/1", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if !strings.Contains(body, `href="/connections?connect=claude-max#add"`) || !strings.Contains(body, "Why it failed") {
		t.Fatal(body)
	}
}
