package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

// diagAPI mounts the rule tester, the stored last event and `explain`.
func (s *server) diagAPI(mux *http.ServeMux) {
	route := func(pattern string, f func(r *http.Request) (any, int, error)) {
		mux.HandleFunc(pattern, s.api(func(w http.ResponseWriter, r *http.Request) { s.reply(w, r, f) }))
	}
	route("POST /api/rules/{name}/test", func(r *http.Request) (any, int, error) {
		rl, ok := s.rule(r.PathValue("name"))
		if !ok {
			return nil, 404, errMsg("no such rule")
		}
		var body struct {
			Event   json.RawMessage   `json:"event"`
			Headers map[string]string `json:"headers"`
			UseLast bool              `json:"use_last"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&body); err != nil {
			return nil, 400, errMsg("bad JSON body")
		}
		var raw string
		if body.UseLast {
			var err error
			if raw, err = store.SourceEvent(s.Store.DB, rl.Source); err != nil {
				return nil, 0, err
			}
			if raw == "" {
				return nil, 404, errMsg("no event stored for source " + rl.Source + " yet")
			}
		} else if len(body.Event) == 0 {
			return nil, 400, errMsg("give an event, or use_last")
		} else {
			raw = string(body.Event)
		}
		data, err := source.DecodeJSON([]byte(raw))
		if err != nil {
			return nil, 400, errMsg("event: " + err.Error())
		}
		headers := map[string]string{}
		for k, v := range body.Headers {
			headers[lower(k)] = v
		}
		return ruleTest(r.Context(), s.Config(), rl, data, headers), 200, nil
	})
	route("GET /api/sources/{name}/last-event", func(r *http.Request) (any, int, error) {
		name := r.PathValue("name")
		if s.Config().Sources[name] == nil {
			return nil, 404, errMsg("no such source")
		}
		raw, err := store.SourceEvent(s.Store.DB, name)
		if err != nil {
			return nil, 0, err
		}
		if raw == "" {
			return nil, 404, errMsg("no event stored for this source yet")
		}
		d, err := store.SourceDiagnostics(s.Store.DB, name)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"source": name, "at": d.EventAt, "event": json.RawMessage(raw)}, 200, nil
	})
	route("GET /api/rules/{name}/explain", func(r *http.Request) (any, int, error) {
		rl, ok := s.rule(r.PathValue("name"))
		if !ok {
			return nil, 404, errMsg("no such rule")
		}
		v, err := s.explain(r, rl)
		return v, 200, err
	})
}

func (s *server) rule(name string) (config.Rule, bool) {
	for _, r := range s.Config().Rules {
		if r.Name == name {
			return r, true
		}
	}
	return config.Rule{}, false
}

type reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type stamped struct {
	At      *time.Time `json:"at"`
	Message string     `json:"message"`
}

// explain answers "why did (or didn't) this rule fire": the facts, and the
// likely reasons, most likely first.
func (s *server) explain(r *http.Request, rl config.Rule) (map[string]any, error) {
	db, cfg, now := s.Store.DB, s.Config(), s.Now()
	off, err := store.RuleOverrides(db)
	if err != nil {
		return nil, err
	}
	fired, err := store.RuleLastFiredAll(db)
	if err != nil {
		return nil, err
	}
	srcs, err := s.sources()
	if err != nil {
		return nil, err
	}
	var src *sourceView
	for i := range srcs {
		if srcs[i].Name == rl.Source {
			src = &srcs[i]
		}
	}
	diag, err := store.SourceDiagnostics(db, rl.Source)
	if err != nil {
		return nil, err
	}
	raw, err := store.SourceEvent(db, rl.Source)
	if err != nil {
		return nil, err
	}
	evalMsg, evalAt, err := store.RuleError(db, rl.Name)
	if err != nil {
		return nil, err
	}
	states, err := store.RuleStates(db, rl.Name)
	if err != nil {
		return nil, err
	}
	recent, err := store.QueryAudit(db, store.AuditFilter{Rule: rl.Name, Limit: 10})
	if err != nil {
		return nil, err
	}
	pend, err := store.PendingApprovals(db)
	if err != nil {
		return nil, err
	}

	out := map[string]any{"rule": rl.Name, "enabled": !off[rl.Name], "overridden": off[rl.Name],
		"last_event_at": diag.EventAt, "recent": recent, "edge_state": []store.RuleStateRow{}}
	if recent == nil {
		out["recent"] = []store.AuditRow{}
	}
	if rl.On != "each" {
		out["edge_state"] = states
		if states == nil {
			out["edge_state"] = []store.RuleStateRow{}
		}
	}
	srcOut := map[string]any{"name": rl.Source}
	if src != nil {
		srcOut["type"], srcOut["health"], srcOut["last_error"], srcOut["last_poll_at"] = src.Type, src.Health, src.LastError, src.LastPollAt
	}
	out["source"] = srcOut
	var lastFired *time.Time
	if t, ok := fired[rl.Name]; ok {
		lastFired = &t
	}
	out["last_fired"] = lastFired
	left := time.Duration(0)
	if lastFired != nil && rl.Cooldown > 0 {
		left = max(0, time.Duration(rl.Cooldown)-now.Sub(*lastFired))
	}
	out["cooldown_left"] = left.Round(time.Second).String()

	// Would the stored event fire the rule from a clean slate?
	var matches *bool
	var test *testView
	if raw != "" {
		if data, derr := source.DecodeJSON([]byte(raw)); derr == nil {
			test = ruleTest(r.Context(), cfg, rl, data, nil)
			m := len(test.Fires) > 0
			matches = &m
			out["test"] = test
		}
	}
	out["last_event_matches"] = matches
	if evalMsg != "" {
		out["last_eval_error"] = stamped{&evalAt, evalMsg}
	}
	if diag.RejectAt != nil {
		out["last_reject"] = stamped{diag.RejectAt, diag.Reject}
	}

	var reasons []reason
	add := func(code, msg string) { reasons = append(reasons, reason{code, msg}) }
	if off[rl.Name] {
		add("disabled", "The rule is disabled (turned off at runtime). Enable it with `siphon enable "+rl.Name+"`.")
	}
	if raw == "" && diag.EventAt == nil {
		add("no_events_yet", "Source "+rl.Source+" has not produced an event yet, so nothing was evaluated.")
	}
	if src != nil && src.LastError != "" {
		add("source_error", "Source "+rl.Source+" is failing: "+src.LastError)
	}
	if diag.RejectAt != nil && (diag.EventAt == nil || diag.RejectAt.After(*diag.EventAt)) {
		add("webhook_rejected", "The last webhook delivery to "+rl.Source+" was refused: "+diag.Reject+". Check the secret and signature settings on the sender.")
	}
	if evalMsg != "" || (test != nil && test.Err != "") {
		m := evalMsg
		if m == "" {
			m = test.Err
		}
		add("eval_error", "The rule's expressions failed on an event: "+m)
	}
	if matches != nil && !*matches && evalMsg == "" && (test == nil || test.Err == "") {
		add("condition_false", "The last event does not satisfy `when` ("+rl.When+").")
	}
	// What the last event did is judged against when it arrived, not against
	// now: an event after the last fire was either held back or latched out.
	eventAfterFire := diag.EventAt != nil && (lastFired == nil || diag.EventAt.After(*lastFired))
	heldBack := false
	if matches != nil && *matches && eventAfterFire {
		if alreadyTrue(rl, test.Fires, states) {
			if rl.On == "each" {
				add("edge_already_true", "This event id already fired the rule (on: each fires once per id).")
			} else {
				add("edge_already_true", "The condition was already true when the last event arrived, and on: edge fires only when it turns true. It fires again after it goes false"+repeatHint(rl)+".")
			}
		}
		if lastFired != nil && rl.Cooldown > 0 {
			gap := diag.EventAt.Sub(*lastFired)
			if cd := time.Duration(rl.Cooldown); gap < cd {
				heldBack = true
				add("cooldown", "The last event arrived "+gap.Round(time.Second).String()+" after the rule fired, inside its "+cd.String()+" cooldown, so it was held back.")
			}
		}
	}
	out["held_back_by_cooldown"] = heldBack
	if lastFired != nil {
		add("fired", "The rule last fired "+now.Sub(*lastFired).Round(time.Second).String()+" ago.")
	}
	for _, p := range pend {
		if p.Rule == rl.Name {
			add("awaiting_approval", "Job "+itoa(int(p.JobID))+" is waiting for approval: `siphon approve "+itoa(int(p.JobID))+"`.")
			break
		}
	}
	if reasons == nil {
		reasons = []reason{}
	}
	out["reasons"] = reasons
	return out, nil
}

func repeatHint(rl config.Rule) string {
	if rl.Repeat > 0 {
		return ", or after repeat (" + time.Duration(rl.Repeat).String() + ")"
	}
	return ""
}

// alreadyTrue is whether the state already covers every key the last event
// would fire: edge keys that are true, or each ids that fired.
func alreadyTrue(rl config.Rule, fires []testFire, states []store.RuleStateRow) bool {
	if len(fires) == 0 {
		return false
	}
	for _, f := range fires {
		hit := false
		for _, st := range states {
			if st.Key == f.Key && ((rl.On == "each" && st.LastFiredAt != nil) || (rl.On != "each" && st.LastValue)) {
				hit = true
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func lower(s string) string { return strings.ToLower(s) }
