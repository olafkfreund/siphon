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

func diagEnv(t *testing.T, rules ...config.Rule) *env {
	return newEnv(t, func(o *Options) {
		o.Cfg.Rules = rules
	})
}

func (e *env) explain(t *testing.T, rule string) (map[string]any, []string) {
	t.Helper()
	w := e.do("GET", "/api/rules/"+rule+"/explain", nil, bearer)
	var got map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("explain: %d %s", w.Code, w.Body.String())
	}
	var codes []string
	for _, r := range got["reasons"].([]any) {
		m := r.(map[string]any)
		if m["message"] == "" {
			t.Fatalf("reason without a message: %v", m)
		}
		codes = append(codes, m["code"].(string))
	}
	return got, codes
}

func hasCode(codes []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, c := range codes {
			found = found || c == w
		}
		if !found {
			return false
		}
	}
	return true
}

func TestExplainReasons(t *testing.T) {
	big := config.Rule{Name: "big", Source: "disk", When: "event.v > 5", Action: config.Action{Cmd: []string{"true"}}}
	each := config.Rule{Name: "each", Source: "disk", When: "event.v > 5", On: "each", ID: "event.v", Action: config.Action{Cmd: []string{"true"}}}
	cool := config.Rule{Name: "cool", Source: "disk", When: "event.v > 5", Cooldown: config.Duration(time.Hour), Action: config.Action{Cmd: []string{"true"}}}
	hook := config.Rule{Name: "hook", Source: "gh", When: "true", Action: config.Action{Cmd: []string{"true"}}}
	e := diagEnv(t, big, each, cool, hook)
	db := e.st.DB
	now := e.now
	event := func(data any) { // an event arriving "now" on the test clock
		store.SetSourceEvent(db, "disk", data)
		db.Exec(`UPDATE source_state SET event_at=? WHERE source='disk'`, now.UnixMilli())
	}

	// no events yet
	if _, c := e.explain(t, "big"); !hasCode(c, "no_events_yet") || hasCode(c, "condition_false") {
		t.Fatalf("no events: %v", c)
	}
	// disabled
	store.SetRuleOverride(db, "big", false, "t", now)
	if got, c := e.explain(t, "big"); !hasCode(c, "disabled") || got["enabled"] != false || got["overridden"] != true {
		t.Fatalf("disabled: %v", c)
	}
	store.SetRuleOverride(db, "big", true, "t", now)

	// condition false
	event(map[string]any{"v": 1})
	if got, c := e.explain(t, "big"); !hasCode(c, "condition_false") || hasCode(c, "no_events_yet", "edge_already_true") || got["last_event_matches"] != false {
		t.Fatalf("condition_false: %v %v", c, got)
	}
	// would fire: no reason against it
	event(map[string]any{"v": 9})
	if got, c := e.explain(t, "big"); len(c) != 0 || got["last_event_matches"] != true {
		t.Fatalf("matching: %v", c)
	}
	// edge already true
	set := func(rule, key string, v bool, fired time.Time) {
		tx, _ := db.Begin()
		if err := store.PutRuleState(tx, rule, key, v, fired); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	}
	set("big", "-", true, now.Add(-time.Minute))
	got, c := e.explain(t, "big")
	if !hasCode(c, "edge_already_true", "fired") || len(got["edge_state"].([]any)) != 1 {
		t.Fatalf("edge: %v %v", c, got["edge_state"])
	}
	// each: already fired for this id
	set("each", "9", true, now.Add(-time.Minute))
	if _, c := e.explain(t, "each"); !hasCode(c, "edge_already_true") {
		t.Fatalf("each: %v", c)
	}
	// cooldown
	set("cool", "-", false, now.Add(-10*time.Minute))
	got, c = e.explain(t, "cool")
	if !hasCode(c, "cooldown", "fired") || hasCode(c, "edge_already_true") || got["cooldown_left"] != "50m0s" {
		t.Fatalf("cooldown: %v %v", c, got["cooldown_left"])
	}
	// source error
	store.PutSourceState(db, "disk", now, "dial tcp: refused")
	if got, c := e.explain(t, "big"); !hasCode(c, "source_error") || got["source"].(map[string]any)["health"] != "error" {
		t.Fatalf("source_error: %v", c)
	}
	// eval error: remembered, and from the stored event itself
	tx, _ := db.Begin()
	store.PutRuleError(tx, "big", now, "boom")
	tx.Commit()
	if got, c := e.explain(t, "big"); !hasCode(c, "eval_error") || got["last_eval_error"].(map[string]any)["message"] != "boom" {
		t.Fatalf("eval_error: %v", c)
	}
	event(map[string]any{"v": "text"})
	if _, c := e.explain(t, "cool"); !hasCode(c, "eval_error") || hasCode(c, "condition_false") {
		t.Fatalf("eval error from event: %v", c)
	}
	// webhook rejected: newer than the last event
	store.SetSourceReject(db, "gh", now, 401, "signature does not match")
	got, c = e.explain(t, "hook")
	if !hasCode(c, "webhook_rejected", "no_events_yet") || !strings.Contains(got["last_reject"].(map[string]any)["message"].(string), "401") {
		t.Fatalf("webhook_rejected: %v", c)
	}
	// awaiting approval
	tx, _ = db.Begin()
	id, _ := store.InsertJob(tx, store.Job{Rule: "hook", ActionJSON: "{}", State: "pending_approval", RunAfter: now}, now)
	store.CreateApproval(tx, id, []byte("h"), now.Add(time.Hour))
	store.Audit(tx, now, "rule:hook", "fire", id, "-")
	tx.Commit()
	got, c = e.explain(t, "hook")
	if !hasCode(c, "awaiting_approval") || len(got["recent"].([]any)) != 1 {
		t.Fatalf("approval: %v %v", c, got["recent"])
	}
	if w := e.do("GET", "/api/rules/nope/explain", nil, bearer); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
}

func TestRuleTestAndLastEventAPI(t *testing.T) {
	big := config.Rule{Name: "big", Source: "disk", When: "event.v > 5", On: "each", ID: "event.v", Action: config.Action{Cmd: []string{"echo", "{{.event.v}}"}}}
	e := diagEnv(t, big)
	post := func(rule, body string) (int, string) {
		w := e.do("POST", "/api/rules/"+rule+"/test", nil, func(r *http.Request) {
			bearer(r)
			r.Body = io.NopCloser(strings.NewReader(body))
			r.ContentLength = int64(len(body))
		})
		return w.Code, w.Body.String()
	}
	code, out := post("big", `{"event":{"v":9,"token":"tok-LEAK"}}`)
	if code != 200 || !strings.Contains(out, `"key":"9"`) || !strings.Contains(out, `"argv":["echo","9"]`) || strings.Contains(out, "tok-LEAK") {
		t.Fatalf("event: %d %s", code, out)
	}
	if code, out = post("big", `{"event":{"v":1}}`); code != 200 || !strings.Contains(out, `"fires":[]`) {
		t.Fatalf("no fire: %d %s", code, out)
	}
	if code, _ = post("big", `{"use_last":true}`); code != 404 {
		t.Fatalf("use_last without event: %d", code)
	}
	if w := e.do("GET", "/api/sources/disk/last-event", nil, bearer); w.Code != 404 {
		t.Fatalf("last-event none: %d", w.Code)
	}
	store.SetSourceEvent(e.st.DB, "disk", map[string]any{"v": 7, "secret": "s-LEAK"})
	if code, out = post("big", `{"use_last":true}`); code != 200 || !strings.Contains(out, `"key":"7"`) || strings.Contains(out, "s-LEAK") {
		t.Fatalf("use_last: %d %s", code, out)
	}
	w := e.do("GET", "/api/sources/disk/last-event", nil, bearer)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"v":7`) || strings.Contains(w.Body.String(), "s-LEAK") || !strings.Contains(w.Body.String(), `"at":`) {
		t.Fatalf("last-event: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "/api/sources/nope/last-event", nil, bearer); w.Code != 404 {
		t.Fatalf("unknown source: %d", w.Code)
	}
	for _, c := range [][2]string{{"nope", `{"event":{}}`}, {"big", `{`}, {"big", `{}`}} {
		if code, _ := post(c[0], c[1]); code != 404 && code != 400 {
			t.Fatalf("%v: %d", c, code)
		}
	}
	if code, _ := post("nope", `{"event":{}}`); code != 404 {
		t.Fatalf("unknown rule: %d", code)
	}
}

func TestAuditFilters(t *testing.T) {
	e := diagEnv(t, config.Rule{Name: "r1", Source: "disk", When: "true", Action: config.Action{Cmd: []string{"true"}}})
	db := e.st.DB
	tx, _ := db.Begin()
	old := e.now.Add(-48 * time.Hour)
	j, _ := store.InsertJob(tx, store.Job{Rule: "r1", ActionJSON: "{}", RunAfter: old}, old)
	store.Audit(tx, old, "rule:r1", "fire", j, "old")
	store.Audit(tx, e.now, "rule:r1", "fire", j, "new")
	store.Audit(tx, e.now, "worker", "job_done", j, "via job")
	store.Audit(tx, e.now, "rule:r2", "fire", 0, "other")
	store.Audit(tx, e.now, "portal", "config_changed", 0, "x")
	tx.Commit()
	count := func(q string) int {
		w := e.do("GET", "/api/audit?"+q, nil, bearer)
		var rows []store.AuditRow
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
		return len(rows)
	}
	for q, want := range map[string]int{"": 5, "rule=r1": 3, "rule=r2": 1, "event=fire": 3, "event=fire&rule=r1": 2, "since=1h": 4, "since=" + e.now.Add(-time.Hour).UTC().Format(time.RFC3339): 4, "rule=r1&since=1h": 2, "rule=none": 0} {
		if got := count(q); got != want {
			t.Errorf("%q: %d rows, want %d", q, got, want)
		}
	}
	if w := e.do("GET", "/api/audit?since=yesterday", nil, bearer); w.Code != 400 {
		t.Fatalf("bad since: %d", w.Code)
	}
}

// A fire at T, an event 22s later inside a 30s cooldown: held back, and still
// explained that way long afterwards (judged against the event, not now).
func TestExplainHeldBackByCooldown(t *testing.T) {
	r := config.Rule{Name: "cd", Source: "disk", When: "event.v > 5", On: "each", ID: "event.v", Cooldown: config.Duration(30 * time.Second), Action: config.Action{Cmd: []string{"true"}}}
	e := diagEnv(t, r)
	db, T := e.st.DB, e.now
	tx, _ := db.Begin()
	store.PutRuleState(tx, "cd", "8", true, T)
	tx.Commit()
	arrive := func(v int, at time.Time) {
		store.SetSourceEvent(db, "disk", map[string]any{"v": v})
		db.Exec(`UPDATE source_state SET event_at=? WHERE source='disk'`, at.UnixMilli())
	}
	// the event that fired it arrived just before the fire: not held back
	arrive(8, T.Add(-time.Second))
	if got, c := e.explain(t, "cd"); hasCode(c, "cooldown") || got["held_back_by_cooldown"] != false || !hasCode(c, "fired") {
		t.Fatalf("firing event: %v", c)
	}
	// the next one, 22s later
	arrive(9, T.Add(22*time.Second))
	e.now = T.Add(25 * time.Second)
	got, c := e.explain(t, "cd")
	var msg string
	for _, r := range got["reasons"].([]any) {
		if m := r.(map[string]any); m["code"] == "cooldown" {
			msg = m["message"].(string)
		}
	}
	if !hasCode(c, "cooldown") || got["held_back_by_cooldown"] != true || !strings.Contains(msg, "22s after the rule fired") || !strings.Contains(msg, "30s cooldown") || !strings.Contains(msg, "held back") {
		t.Fatalf("held back: %v %q", c, msg)
	}
	// minutes later the cooldown is over, but the last event was still held back
	e.now = T.Add(10 * time.Minute)
	got, c = e.explain(t, "cd")
	if !hasCode(c, "cooldown") || got["cooldown_left"] != "0s" {
		t.Fatalf("later: %v %v", c, got["cooldown_left"])
	}
	// an event after the cooldown would fire: nothing holds it back
	arrive(10, T.Add(time.Minute))
	if got, c = e.explain(t, "cd"); hasCode(c, "cooldown") || got["held_back_by_cooldown"] != false {
		t.Fatalf("after the window: %v", c)
	}
}
