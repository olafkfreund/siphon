package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func scheduleEnv(t *testing.T) *env {
	return newEnv(t, func(o *Options) {
		o.Cfg.Sources["tick"] = &config.Source{Type: "schedule", At: "0 6 * * *", Timezone: "Europe/London", Data: map[string]any{"who": "w"}}
		o.Cfg.Rules = []config.Rule{{Name: "daily", Source: "tick", When: "true", On: "each", ID: "event.scheduled_at", Action: config.Action{Cmd: []string{"echo", "{{.event.who}}"}}}}
	})
}

func TestScheduleSourceView(t *testing.T) {
	e := scheduleEnv(t)
	last := time.Date(2027, 1, 15, 6, 0, 0, 0, time.UTC)
	store.PutSourceState(e.st.DB, "tick", last, "")
	store.SetSourceEvent(e.st.DB, "tick", map[string]any{"scheduled_at": "2027-01-15T06:00:00Z"})
	w := e.do("GET", "/api/sources", nil, bearer)
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var tick map[string]any
	for _, r := range rows {
		if r["name"] == "tick" {
			tick = r
		}
	}
	if tick == nil || tick["next_run_at"] != "2027-01-16T06:00:00Z" || tick["last_run_at"] != "2027-01-15T06:00:00Z" || tick["timezone"] != "Europe/London" || tick["health"] != "ok" {
		t.Fatalf("%v", tick)
	}
	if rows[0]["name"] != "disk" || rows[0]["next_run_at"] != nil {
		t.Errorf("other sources must not have next_run_at: %v", rows[0])
	}
	// the portal page
	c, _ := e.login()
	page := e.do("GET", "/sources", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if !strings.Contains(page, "Next run 2027-01-16 06:00 Europe/London") || !strings.Contains(page, "last run 2027-01-15 06:00 Europe/London") {
		t.Errorf("sources page: %s", page)
	}
}

func TestScheduleTestAtAndWhy(t *testing.T) {
	e := scheduleEnv(t)
	post := func(body string) (int, map[string]any) {
		w := e.do("POST", "/api/rules/daily/test", nil, func(r *http.Request) { bearer(r); r.Body = io.NopCloser(strings.NewReader(body)) })
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m
	}
	code, m := post(`{"at":"2027-03-02 06:00"}`)
	if code != 200 || !strings.Contains(m["event"].(string), `"scheduled_at": "2027-03-02T06:00:00Z"`) || len(m["fires"].([]any)) != 1 {
		t.Fatalf("test --at: %d %v", code, m)
	}
	if code, _ = post(`{"at":"nonsense"}`); code != 400 {
		t.Errorf("bad time: %d", code)
	}
	// a rule on a non-schedule source refuses --at
	e.h = New(Options{Token: tok, Store: e.st, Now: func() time.Time { return e.now }, Cfg: &config.Config{
		Sources: map[string]*config.Source{"disk": {Type: "http"}},
		Rules:   []config.Rule{{Name: "daily", Source: "disk", When: "true", Action: config.Action{Cmd: []string{"true"}}}}}})
	if code, _ = post(`{"at":"2027-03-02 06:00"}`); code != 400 {
		t.Errorf("--at on http source: %d", code)
	}
}

func TestScheduleWhyShowsMissed(t *testing.T) {
	e := scheduleEnv(t)
	store.PutSourceState(e.st.DB, "tick", e.now, "")
	store.SetSourceEvent(e.st.DB, "tick", map[string]any{"scheduled_at": "2027-01-15T06:00:00Z"})
	tx, _ := e.st.DB.Begin()
	store.Audit(tx, e.now, "system", "schedule_skipped", 0, `{"source":"tick","count":3,"from":"2027-01-13T06:00:00Z","to":"2027-01-14T06:00:00Z"}`)
	tx.Commit()
	got, _ := e.explain(t, "daily")
	sch := got["source"].(map[string]any)["schedule"].(map[string]any)
	if sch["next_run_at"] == nil || sch["last_run_at"] != "2027-01-15T06:00:00Z" || sch["timezone"] != "Europe/London" {
		t.Fatalf("%v", sch)
	}
	if m, _ := sch["missed"].(map[string]any); m == nil || m["count"] != float64(3) {
		t.Fatalf("missed: %v", sch)
	}
}
