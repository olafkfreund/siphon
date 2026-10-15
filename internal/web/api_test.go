package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// Plan step 3: /api/jobs/{id}?detail=1 is the portal's job detail, and masks as it does.
func TestAPIJobDetail(t *testing.T) {
	cfgSecret := "cfg-secret-" + "value-1"
	e := newEnv(t, func(o *Options) {
		o.Cfg.Sources["gh"].Secret = config.Secret{Ref: "env:X", Value: cfgSecret}
		o.Cfg.Agents = map[string]*config.Agent{"helper": {Kind: "claude", Prompt: "Answer: {{.event.q}}"}}
	})
	tx, _ := e.st.DB.Begin()
	id, err := store.InsertJob(tx, store.Job{Rule: "disk-full", State: "running",
		ActionJSON: `{"action":{"Routine":"fix"},"env":{"event":{"q":"hi","api_key":"sk-SECRETVALUE"}},` +
			`"steps":[{"id":"a","cmd":["echo","one"]},{"id":"b","agent":"helper"}]}`}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := store.InsertJob(tx, store.Job{Rule: "other", State: "pending_approval", ActionJSON: `{"action":{"Cmd":["true"]}}`}, e.now)
	store.CreateApproval(tx, pid, []byte("h"), e.now.Add(time.Hour))
	tx.Commit()
	store.FinishJob(e.st.DB, id, "failed", 1, "boom\n", e.now)

	w := e.do("GET", "/api/jobs/1?detail=1", nil, bearer)
	var got struct {
		Kind  string `json:"kind"`
		Steps []struct{ ID, Kind, Target string }
		Event string `json:"event"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Kind != "routine" || len(got.Steps) != 2 || got.Steps[1].Kind != "agent" {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	for _, s := range []string{"SECRETVALUE", cfgSecret} {
		if strings.Contains(w.Body.String(), s) {
			t.Errorf("response shows %q", s)
		}
	}
	if w = e.do("GET", "/api/jobs/99?detail=1", nil, bearer); w.Code != 404 {
		t.Errorf("missing job: %d", w.Code)
	}

	w = e.do("GET", "/api/approvals", nil, bearer)
	var rows []struct {
		JobID        int64 `json:"job_id"`
		Kind, Target string
	}
	if json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 || rows[0].Kind != "cmd" || rows[0].Target != "true" {
		t.Fatalf("approvals: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"source_type"`) || !strings.Contains(w.Body.String(), `"job_id"`) {
		t.Errorf("approval fields: %s", w.Body.String())
	}
}

// Plan step 4: /api/egress is the Egress page's data.
func TestAPIEgress(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Agents = map[string]*config.Agent{"helper": {Kind: "claude", Prompt: "x"}}
	})
	w := e.do("GET", "/api/egress", nil, bearer)
	var got struct {
		Lists []struct {
			Owner, Name string
			Hosts       []struct{ Host string }
		}
		Blocked []struct{ Host string }
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Lists) == 0 || got.Lists[0].Owner != "agent" || got.Lists[0].Name != "helper" {
		t.Fatalf("egress: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"blocked"`) {
		t.Errorf("no blocked key: %s", w.Body.String())
	}
	if w = e.do("GET", "/api/egress", nil, nil); w.Code != 401 {
		t.Errorf("no token: %d", w.Code)
	}
}

// Plan step 6: /api/dashboard is the dashboard's last-24-hours numbers; the clock is s.Now.
func TestAPIDashboard(t *testing.T) {
	e := newEnv(t, nil)
	tx, _ := e.st.DB.Begin()
	for _, st := range []string{"done", "done", "done", "failed", "running"} {
		store.InsertJob(tx, store.Job{Rule: "other", State: st, ActionJSON: `{"action":{"Cmd":["true"]}}`}, e.now)
	}
	tx.Commit()
	var got struct {
		Total, Running, Failed int
		SuccessRate            string `json:"success_rate"`
		Counts                 map[string]int
	}
	w := e.do("GET", "/api/dashboard", nil, bearer)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Total != 5 || got.Running != 1 || got.Failed != 1 || got.SuccessRate != "75.0%" || got.Counts["done"] != 3 {
		t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
	}
	// a day later the jobs fall out of the window
	e.now = e.now.Add(25 * time.Hour)
	w = e.do("GET", "/api/dashboard", nil, bearer)
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Total != 0 {
		t.Fatalf("after 25h: %s", w.Body.String())
	}
}
