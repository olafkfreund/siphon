package web

import (
	"html"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func jobsCount(ce *cfgEnv) (n int) {
	ce.st.DB.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n)
	return
}

// ruleForm is the rule editor's form as the browser sends it.
func ruleForm(extra url.Values) url.Values {
	v := url.Values{"mode": {"form"}, "name": {"t"}, "f.source": {"gh"}, "f.when": {`event.kind == "ask"`}, "f.on": {"each"},
		"f.id": {"event.id"}, "action_kind": {"agent"}, "f.action.agent": {"helper"}, "event_src": {"paste"},
		"event_json": {`{"id": 7, "kind": "ask", "q": "What is 2+2?", "api_key": "sk-XYZ"}`}}
	for k, e := range extra {
		v[k] = e
	}
	return v
}

func TestRuleTesterNeverEnqueues(t *testing.T) {
	ce := newCfgEnv(t)
	if w := ce.saveYAML("agents", "helper", `{kind: claude, prompt: "Answer: {{.event.q}}", max_turns: 2}`); w.Code != 303 {
		t.Fatalf("agent: %d %s", w.Code, w.Body.String())
	}
	applied, revs := ce.applied.Load(), ce.latest()

	w := ce.post("/rules/test", ruleForm(nil))
	body := html.UnescapeString(w.Body.String())
	for _, want := range []string{"Matches", "Answer: What is 2+2?", "helper", "max 2 turns", "waits for approval", "[redacted]"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "sk-XYZ") {
		t.Error("event secret shown")
	}
	// Same again, and a no-match and a command rule: still nothing enqueued or applied.
	ce.post("/rules/test", ruleForm(url.Values{"f.when": {"false"}}))
	cmd := ce.post("/rules/test", ruleForm(url.Values{"action_kind": {"cmd"}, "f.action.cmd": {"echo\nhi {{.event.q}}"}}))
	if !strings.Contains(html.UnescapeString(cmd.Body.String()), "echo hi What is 2+2?") {
		t.Errorf("argv not rendered: %s", cmd.Body.String())
	}
	if w := ce.post("/rules/test", ruleForm(url.Values{"f.when": {"false"}})); !strings.Contains(w.Body.String(), "No match") {
		t.Errorf("no-match not shown: %s", w.Body.String())
	}
	if jobsCount(ce) != 0 || ce.applied.Load() != applied || ce.latest() != revs {
		t.Fatalf("tester had side effects: jobs=%d applied=%d rev=%d", jobsCount(ce), ce.applied.Load(), ce.latest())
	}
	if n, _ := store.ConfigItems(ce.st.DB); len(n) != 1 {
		t.Fatalf("tester changed the overlay: %+v", n)
	}
}

func TestRuleTesterEvents(t *testing.T) {
	ce := newCfgEnv(t)
	ce.saveYAML("agents", "helper", `{kind: claude, prompt: "Q: {{.event.q}}"}`)
	// No stored event and nothing pasted.
	last := ruleForm(url.Values{"event_src": {"last"}})
	if w := ce.post("/rules/test", last); !strings.Contains(w.Body.String(), "No event yet: paste one") {
		t.Fatalf("no event: %s", w.Body.String())
	}
	if w := ce.post("/rules/test", ruleForm(url.Values{"event_json": {""}})); !strings.Contains(w.Body.String(), "No event yet: paste one") {
		t.Fatalf("empty paste: %s", w.Body.String())
	}
	if w := ce.post("/rules/test", ruleForm(url.Values{"event_json": {"{nope"}})); !strings.Contains(w.Body.String(), "event:") {
		t.Fatalf("bad json: %s", w.Body.String())
	}
	// The source's last event is the default.
	store.SetSourceEvent(ce.st.DB, "gh", map[string]any{"id": 1, "kind": "ask", "q": "from the store", "token": "tok-LEAK"}, nil)
	w := ce.post("/rules/test", last)
	if !strings.Contains(html.UnescapeString(w.Body.String()), "Q: from the store") || strings.Contains(w.Body.String(), "tok-LEAK") {
		t.Fatalf("last event: %s", w.Body.String())
	}
	// Headers reach the expression, lower-cased.
	h := ruleForm(url.Values{"f.when": {`headers["x-kind"] == "ask"`}, "headers_json": {`{"X-Kind": "ask"}`}})
	if w := ce.post("/rules/test", h); !strings.Contains(w.Body.String(), "Matches") {
		t.Fatalf("headers: %s", w.Body.String())
	}
	if w := ce.do("POST", "/rules/test", ruleForm(nil), nil); w.Code != 401 {
		t.Fatalf("no session: %d", w.Code)
	}
	bad := ruleForm(nil)
	bad.Set("csrf", "nope")
	if w := ce.post("/rules/test", bad); w.Code != 403 {
		t.Fatalf("bad csrf: %d", w.Code)
	}
}

func TestEgressPreviewMatchesAgentEgress(t *testing.T) {
	ce := newCfgEnv(t)
	cfg := ce.cur.Load()
	hosts, on := cfg.AgentEgress(&config.Agent{Kind: "claude", Egress: config.EgressAgent{Allow: []string{"extra.example.com"}}})
	if !on || len(hosts) < 2 {
		t.Fatalf("setup: %v %v", hosts, on)
	}
	w := ce.post("/agents/egress-preview", url.Values{"mode": {"form"}, "name": {"x"}, "f.kind": {"claude"}, "f.egress.allow": {"extra.example.com"}})
	for _, h := range hosts {
		if want := h.Host + ":" + strconv.Itoa(h.Port); !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s in %s", want, w.Body.String())
		}
	}
	off := ce.post("/agents/egress-preview", url.Values{"mode": {"form"}, "name": {"x"}, "f.kind": {"claude"}, "f.egress.enabled": {"false"}})
	if !strings.Contains(off.Body.String(), "Restriction is off") {
		t.Fatalf("disabled: %s", off.Body.String())
	}
	if ce.latest() != 0 || ce.applied.Load() != 0 {
		t.Fatal("preview saved something")
	}
}

func TestEditorPages(t *testing.T) {
	ce := newCfgEnv(t)
	ce.saveYAML("routines", "ro", `{steps: [{id: build, cmd: [make]}, {id: ship, cmd: [deploy], approve: true, if: "steps.build.exit == 0"}]}`)
	store.SetSourceEvent(ce.st.DB, "gh", map[string]any{"n": 1, "secret": "s-LEAK"}, nil)
	if b := ce.get("/config/routines/ro").Body.String(); !strings.Contains(b, "build") || !strings.Contains(b, "ship") || !strings.Contains(b, "approval") {
		t.Errorf("step list: %s", b)
	}
	if b := ce.get("/config/sources/gh").Body.String(); !strings.Contains(b, "[redacted]") || strings.Contains(b, "s-LEAK") || !strings.Contains(b, "Health") {
		t.Errorf("source editor: %s", b)
	}
	b := ce.get("/config/rules/r1").Body.String()
	for _, want := range []string{`hx-post="/rules/test"`, "input changed delay:400ms from:closest form, load", `name="action_kind"`, `value="gh" selected`} {
		if !strings.Contains(b, want) {
			t.Errorf("rule editor missing %q", want)
		}
	}
	if b := ce.get("/config/agents/new").Body.String(); !strings.Contains(b, `hx-post="/agents/egress-preview"`) {
		t.Error("agent editor has no egress preview")
	}
	if b := ce.get("/rules").Body.String(); !strings.Contains(b, "/config/rules/new") || !strings.Contains(b, "/config/rules/r1") {
		t.Error("rules list has no New/Edit links")
	}
}

// Each editor's form maps an item to YAML and back without changing it.
func TestEditorsRoundTrip(t *testing.T) {
	items := []struct{ kind, y string }{
		{"rules", "source: gh\nwhen: event.n > 1\non: each\nid: event.id\nfor_each: event.items\nrepeat: 1h\ncooldown: 30s\napprove: true\naction:\n  cmd: [echo, \"{{.event.n}}\"]\negress:\n  enabled: true\n"},
		{"rules", "source: gh\nwhen: \"true\"\naction:\n  agent: helper\n"},
		{"sources", "type: http\nurl: https://example.com/x\npoll: 5m\nallow_private: true\nmethod: POST\nbody: |\n  {\"a\": 1}\nheaders:\n  X-Key: env:KEY\n"},
		{"sources", "type: webhook\nsecret: env:AGW_HOOK\nsignature: sha256\nsignature_header: X-Sig\ntimestamp_header: X-Ts\nid: header.X-Id\n"},
		{"sources", "type: mcp\ncommand: [srv, --flag]\nread:\n  resource: test://v\n  args:\n    a: 1\nauth:\n  bearer: file:/run/secrets/tok\npoll: 1m\n"},
		{"agents", "kind: codex\ncredential: c1\ncommand: /bin/codex\nprompt: |\n  Do the thing\n  twice\nmcp: [a, b]\nallowed_tools: [Read, Grep]\nmax_turns: 3\nmax_budget_usd: 0.25\ntimeout: 10m\napprove: false\negress:\n  enabled: true\n  allow: [x.example.com, y.example.com]\n"},
		{"credentials", "provider: openai\nurl: https://api.groq.com/openai/v1\npreset: groq\nconcurrency: 1\napi_key: file:/run/secrets/k\n"},
		{"credentials", "provider: claude\nconcurrency: 2\napi_key: file:/run/secrets/k\n"},
	}
	for _, it := range items {
		want := decodeMap(it.y)
		fields := formFields(it.kind, it.y)
		form := url.Values{}
		for _, f := range fields {
			switch f.Kind {
			case "check":
				if f.Checked {
					form.Set(f.Name, "on")
				}
			case "secret": // never rendered back: an empty field keeps the stored ref
			default:
				form.Set(f.Name, f.Value)
			}
		}
		if it.kind == "rules" {
			form.Set("action_kind", map[bool]string{true: "cmd", false: "agent"}[form.Get("f.action.cmd") != ""])
		}
		got, _, err := applyForm(it.kind, "x", it.y, form, "/nonexistent/s.db")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decodeMap(got), want) {
			t.Errorf("%s round trip changed the item:\n--- before\n%s--- after\n%s", it.kind, it.y, got)
		}
	}
}

func TestRoutineStarterParses(t *testing.T) {
	var rt config.Routine
	if err := yaml.Unmarshal([]byte(routineStarter), &rt); err != nil || len(rt.Steps) != 2 || rt.Steps[0].ID != "fetch" || rt.Steps[1].Agent == "" {
		t.Fatalf("starter: %v %+v", err, rt)
	}
}

func TestRoutineEditorRecipesAndTokens(t *testing.T) {
	ce := newCfgEnv(t)
	body := func(path string) string { return html.UnescapeString(ce.get(path).Body.String()) }

	b := body("/config/routines/new")
	for _, want := range []string{"Start from a recipe", "/config/routines/new?recipe=", "{{.steps.fetch.output}}", "{{.steps.summarise.output}}", "{{.event.FIELD}}", `data-insert=`, "<h2>Routine YAML</h2>", "steps:"} {
		if !strings.Contains(b, want) {
			t.Errorf("new routine: missing %q", want)
		}
	}
	if strings.Contains(b, "<details") {
		t.Error("routine form is collapsed")
	}

	r := recipeNameForTest(t)
	b = body("/config/routines/new?recipe=" + r.Name)
	if !strings.Contains(b, "/help/templates#"+r.Name) || !strings.Contains(b, `value="`+r.routine+`"`) {
		t.Errorf("recipe %s not prefilled:\n%s", r.Name, b)
	}
	if b = body("/config/routines/new?recipe=nope"); strings.Contains(b, "/help/templates#") || !strings.Contains(b, "CHANGE-ME") || !strings.Contains(b, "Unknown recipe") {
		t.Error("unknown recipe prefilled, or not said")
	}

	// the "new" page never replaces an existing routine (a recipe prefills a name that may be taken)
	form := url.Values{"mode": {"yaml"}, "name": {"dup"}, "rev": {"0"}, "yaml": {"steps:\n  - id: a\n    cmd: [echo, one]\n"}}
	if w := ce.post("/config/routines/new/save", form); w.Code != 303 {
		t.Fatalf("first save: %d %s", w.Code, w.Body.String())
	}
	form.Set("rev", strconv.FormatInt(ce.latest(), 10))
	form.Set("yaml", "steps:\n  - id: b\n    cmd: [echo, two]\n")
	if w := ce.post("/config/routines/new/save", form); w.Code != 409 || !strings.Contains(w.Body.String(), "already exists") {
		t.Fatalf("overwrite from new: %d", w.Code)
	}
	if rt := ce.cur.Load().Routines["dup"]; rt == nil || len(rt.Steps) != 1 || rt.Steps[0].ID != "a" {
		t.Fatalf("routine was replaced: %+v", rt)
	}
	// a page with a form keeps the collapsible YAML editor and its live validation
	if b = body("/config/rules/r1"); !strings.Contains(b, "<details") || !strings.Contains(b, `/validate"`) {
		t.Error("rule editor lost its YAML form")
	}
}

func TestStepTokensQuoteNonIdentifiers(t *testing.T) {
	if got := stepTokens("fetch-data"); got[0] != `{{index .steps "fetch-data" "output"}}` {
		t.Fatalf("%q", got)
	}
	if got := stepTokens("fetch"); got[0] != "{{.steps.fetch.output}}" {
		t.Fatalf("%q", got)
	}
}

type testRecipe struct{ Name, routine string }

func recipeNameForTest(t *testing.T) testRecipe {
	t.Helper()
	for _, r := range routineRecipes() {
		if rv, name, _, ok := recipeFor(r.Name); ok && rv != nil {
			return testRecipe{r.Name, name}
		}
	}
	t.Fatal("no template with a routine")
	return testRecipe{}
}
