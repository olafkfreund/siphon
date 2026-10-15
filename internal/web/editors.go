package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/docs"
	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

type optPair struct{ Name, Type string }

type stepRow struct{ ID, Kind, Target, Flags string }

// routineStarter is what a new, empty routine begins with.
const routineStarter = `# A routine runs its steps in order. A later step, or an agent's prompt,
# reads an earlier one as {{.steps.<id>.output}}.
steps:
  - id: fetch
    cmd: [curl, -fsS, https://example.com/status.json]
  - id: summarise
    agent: CHANGE-ME   # an agent whose prompt uses {{.steps.fetch.output}}
`

// recipe is a template that has a routine, offered as a starting point.
type recipe struct{ Name, Title string }

// recipeView is the chosen recipe: what was prefilled and what else it brings.
type recipeView struct {
	Name, Title string
	Others      []string // the template's other items, as kind/name
}

// routineRecipes are the templates with a routines: section.
func routineRecipes() (out []recipe) {
	for _, t := range docs.Templates() {
		if _, _, ok := templateRoutine(t.YAML); ok {
			out = append(out, recipe{t.Name, t.Title})
		}
	}
	return out
}

// templateRoutine is routines[0] of an apply file: its name and body as YAML.
func templateRoutine(src string) (name, body string, ok bool) {
	var doc struct {
		Routines yaml.Node `yaml:"routines"`
	}
	n := &doc.Routines
	if yaml.Unmarshal([]byte(src), &doc) != nil || n.Kind != yaml.MappingNode || len(n.Content) < 2 {
		return "", "", false
	}
	b, err := yaml.Marshal(n.Content[1])
	return n.Content[0].Value, string(b), err == nil
}

// recipeFor prefills a new routine from the named template; unknown gives ok=false.
func recipeFor(name string) (rv *recipeView, routine, body string, ok bool) {
	for _, t := range docs.Templates() {
		if t.Name != name {
			continue
		}
		if routine, body, ok = templateRoutine(t.YAML); !ok {
			return nil, "", "", false
		}
		rv = &recipeView{Name: t.Name, Title: t.Title}
		var all map[string]yaml.Node
		if yaml.Unmarshal([]byte(t.YAML), &all) != nil {
			return rv, routine, body, true
		}
		for _, kind := range sortedKeys(all) {
			n := all[kind]
			switch n.Kind {
			case yaml.MappingNode:
				for i := 0; i+1 < len(n.Content); i += 2 {
					if kind != "routines" || n.Content[i].Value != routine {
						rv.Others = append(rv.Others, kind+"/"+n.Content[i].Value)
					}
				}
			case yaml.SequenceNode:
				for _, it := range n.Content {
					var r struct{ Name string }
					if it.Decode(&r) == nil && r.Name != "" {
						rv.Others = append(rv.Others, kind+"/"+r.Name)
					}
				}
			}
		}
		return rv, routine, body, true
	}
	return nil, "", "", false
}

// editExtras fills what the per-kind editors show next to the form: pickers,
// the source's health and last event, a routine's step list.
func (s *server) editExtras(v *cfgView) error {
	cfg := s.Config()
	v.Base = "/config/" + v.Kind + "/new"
	if !v.New {
		v.Base = "/config/" + v.Kind + "/" + url.PathEscape(v.Name)
	}
	switch v.Kind {
	case "rules":
		v.Side = "rules"
		v.F = map[string]fieldView{}
		for _, f := range v.Fields {
			v.F[f.Key] = f
		}
		v.ActionKind = "agent"
		for _, k := range []string{"cmd", "agent", "unit", "routine"} {
			if f := v.F["action."+k]; f.Value != "" {
				v.ActionKind = k
			}
		}
		for _, n := range sortedKeys(cfg.Sources) {
			v.SourceOpts = append(v.SourceOpts, optPair{n, cfg.Sources[n].Type})
		}
		v.AgentOpts, v.UnitOpts, v.RoutineOpt = sortedKeys(cfg.Agents), append([]string(nil), cfg.Units...), sortedKeys(cfg.Routines)
	case "sources":
		v.Side = "sources"
		if !v.New {
			srcs, err := s.sources()
			if err != nil {
				return err
			}
			for i := range srcs {
				if srcs[i].Name == v.Name {
					v.Health = &srcs[i]
				}
			}
			raw, err := store.SourceEvent(s.Store.DB, v.Name)
			if err != nil {
				return err
			}
			v.LastEvent = prettyJSON(raw)
		}
	case "agents":
		v.Side = "agents"
	case "routines":
		v.Side = "routines"
		if v.New && strings.TrimSpace(v.YAML) == "" {
			v.YAML = routineStarter
		}
		v.Recipes = routineRecipes()
		_, v.RefFields, _ = config.Explain("routine")
		var rt config.Routine
		if yaml.Unmarshal([]byte(v.YAML), &rt) == nil {
			for _, st := range rt.Steps {
				r := stepRow{ID: st.ID}
				switch {
				case st.Agent != "":
					r.Kind, r.Target = "agent", st.Agent
				case st.Unit != "":
					r.Kind, r.Target = "unit", st.Unit
				default:
					r.Kind, r.Target = "cmd", strings.Join(st.Cmd, " ")
				}
				var fl []string
				for _, f := range []struct {
					on   bool
					name string
				}{{st.If != "", "if " + st.If}, {st.Retry != nil, "retry"}, {st.Timeout > 0, "timeout " + time.Duration(st.Timeout).String()},
					{st.Approve, "approval"}, {st.ContinueOnError, "continue on error"}} {
					if f.on {
						fl = append(fl, f.name)
					}
				}
				r.Flags = strings.Join(fl, " · ")
				v.Steps = append(v.Steps, r)
				if st.ID != "" {
					v.Tokens = append(v.Tokens, stepTokens(st.ID)...)
				}
			}
		}
		v.Tokens = append(v.Tokens, "{{.event.FIELD}}")
	}
	return nil
}

// snTable is the table a servicenow action touches.
func snTable(a *config.ServiceNowAction) string {
	switch {
	case a.Op == "create_change":
		return "change_request"
	case a.Table == "":
		return "incident"
	}
	return a.Table
}

// snArgv shows a servicenow action as the sys_id and k=v fields, rendered with env.
func snArgv(a *config.ServiceNowAction, env map[string]any) ([]string, error) {
	var out []string
	if a.SysID != "" {
		id, err := action.RenderPrompt(a.SysID, env)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	for _, k := range sortedKeys(a.Fields) {
		v, err := action.RenderPrompt(a.Fields[k], env)
		if err != nil {
			return nil, err
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// hasServiceNow reports whether the item YAML (a rule or a routine) has a
// servicenow action, which the field form cannot show or keep.
func hasServiceNow(kind, y string) bool {
	switch kind {
	case "rules":
		var r config.Rule
		return yaml.Unmarshal([]byte(y), &r) == nil && r.Action.ServiceNow != nil
	case "routines":
		var rt config.Routine
		if yaml.Unmarshal([]byte(y), &rt) == nil {
			for _, st := range rt.Steps {
				if st.ServiceNow != nil {
					return true
				}
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func prettyJSON(raw string) string {
	var v any
	if raw == "" || json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	b, _ := json.MarshalIndent(store.RedactKeys(v), "", "  ")
	return string(b)
}

// testView is the live tester's answer. The tester evaluates a candidate rule
// against an event on a throwaway in-memory store: nothing is enqueued, run or applied.
type testView struct {
	Err      string     `json:"error,omitempty"`
	NoEvent  bool       `json:"no_event,omitempty"`
	Event    string     `json:"event,omitempty"` // redacted, as shown
	Fires    []testFire `json:"fires"`
	Approval bool       `json:"needs_approval"`
}

type testFire struct {
	Key    string   `json:"key"`
	Item   any      `json:"item,omitempty"` // the for_each item, redacted
	Kind   string   `json:"kind,omitempty"`
	Target string   `json:"target,omitempty"`
	Info   string   `json:"info,omitempty"`
	Prompt string   `json:"prompt,omitempty"`
	Err    string   `json:"error,omitempty"`
	Argv   []string `json:"argv,omitempty"`
}

func (s *server) ruleTest(w http.ResponseWriter, r *http.Request, _ string) {
	s.render(w, "tester", s.runRuleTest(r))
}

func (s *server) runRuleTest(r *http.Request) *testView {
	tv := &testView{}
	cfg := s.Config()
	name := r.PostFormValue("name")
	y := r.PostFormValue("yaml")
	if r.PostFormValue("mode") != "yaml" {
		var err error
		if y, _, err = applyForm("rules", name, "", r.PostForm, cfg.Server.DB); err != nil {
			tv.Err = err.Error()
			return tv
		}
	}
	rl, perr := parseRule(cfg, name, y)
	if perr != nil {
		tv.Err = perr.Error()
		return tv
	}
	var data any
	var headers map[string]string
	if r.PostFormValue("event_src") == "paste" {
		raw := strings.TrimSpace(r.PostFormValue("event_json"))
		if raw == "" {
			tv.NoEvent = true
			return tv
		}
		var err error
		if data, err = source.DecodeJSON([]byte(raw)); err != nil {
			tv.Err = "event: " + err.Error()
			return tv
		}
	} else {
		raw, err := store.SourceEvent(s.Store.DB, rl.Source)
		if err != nil {
			tv.Err = "internal error"
			return tv
		}
		if raw == "" {
			tv.NoEvent = true
			return tv
		}
		if data, err = source.DecodeJSON([]byte(raw)); err != nil {
			tv.Err = "stored event: " + err.Error()
			return tv
		}
		if headers, err = store.SourceEventHeaders(s.Store.DB, rl.Source); err != nil {
			tv.Err = "internal error"
			return tv
		}
	}
	// form headers win, key by key, over the stored ones
	if h := strings.TrimSpace(r.PostFormValue("headers_json")); h != "" {
		var in map[string]string
		if err := json.Unmarshal([]byte(h), &in); err != nil {
			tv.Err = "headers: want a JSON object of strings"
			return tv
		}
		if headers == nil {
			headers = map[string]string{}
		}
		for k, v := range in {
			headers[strings.ToLower(k)] = v
		}
	}
	return ruleTest(r.Context(), cfg, rl, data, headers)
}

// parseRule reads rule YAML that is not saved: it must parse and name a known source.
func parseRule(cfg *config.Config, name, y string) (config.Rule, error) {
	var rl config.Rule
	if err := yaml.Unmarshal([]byte(y), &rl); err != nil {
		return rl, errors.New("rule: " + err.Error())
	}
	rl.Name = name
	if rl.Source == "" || cfg.Sources[rl.Source] == nil {
		return rl, errors.New("Pick a source.")
	}
	return rl, nil
}

// ruleTest evaluates rl against one event on a throwaway store and renders
// what each fire would do. Nothing is enqueued, run or applied.
func ruleTest(ctx context.Context, cfg *config.Config, rl config.Rule, data any, headers map[string]string) *testView {
	tv := &testView{Fires: []testFire{}}
	if b, err := json.MarshalIndent(store.RedactKeys(data), "", "  "); err == nil {
		tv.Event = string(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	fires, err := rule.DryRun(ctx, rl, rule.Event{Source: rl.Source, Headers: headers, Data: data})
	if err != nil {
		tv.Err = err.Error()
	}
	tv.Approval = cfg.NeedsApproval(rl)
	for _, f := range fires {
		tf := testFire{Key: f.Key}
		if f.Item != nil {
			tf.Item = store.RedactKeys(f.Item)
		}
		switch a := rl.Action; {
		case a.Agent != "":
			tf.Kind, tf.Target = "agent", a.Agent
			def := cfg.Agents[a.Agent]
			if def == nil {
				tf.Err = "unknown agent " + a.Agent
				break
			}
			tf.Info = def.Kind
			if def.MaxTurns > 0 {
				tf.Info += ", max " + strconv.Itoa(def.MaxTurns) + " turns"
			}
			if p, err := action.RenderPrompt(def.Prompt, f.Env); err != nil {
				tf.Err = err.Error()
			} else {
				tf.Prompt = p
			}
		case len(a.Cmd) > 0:
			tf.Kind, tf.Target = "command", a.Cmd[0]
			if tf.Argv, err = action.Render(a.Cmd, f.Env); err != nil {
				tf.Err = err.Error()
			}
		case a.Unit != "":
			tf.Kind, tf.Target = "unit", a.Unit
		case a.ServiceNow != nil:
			tf.Kind, tf.Target = "servicenow", a.ServiceNow.Op+" "+snTable(a.ServiceNow)
			if tf.Argv, err = snArgv(a.ServiceNow, f.Env); err != nil {
				tf.Err = err.Error()
			}
		case a.Routine != "":
			tf.Kind, tf.Target = "routine", a.Routine
			if rt := cfg.Routines[a.Routine]; rt != nil {
				tf.Info = strconv.Itoa(len(rt.Steps)) + " steps"
			}
		}
		tv.Fires = append(tv.Fires, tf)
	}
	return tv
}

// egressPreview is the agent editor's live allowlist.
type egressPreview struct {
	Err   string
	On    bool
	Hosts []config.HostPort
}

func (s *server) egressPreview(w http.ResponseWriter, r *http.Request, _ string) {
	cfg := s.Config()
	ev := &egressPreview{}
	y := r.PostFormValue("yaml")
	if r.PostFormValue("mode") != "yaml" {
		var err error
		if y, _, err = applyForm("agents", r.PostFormValue("name"), "", r.PostForm, cfg.Server.DB); err != nil {
			ev.Err = err.Error()
			s.render(w, "egresspreview", ev)
			return
		}
	}
	var a config.Agent
	if err := yaml.Unmarshal([]byte(y), &a); err != nil {
		ev.Err = "agent: " + err.Error()
	} else {
		if a.Kind == "" {
			a.Kind = "claude"
		}
		ev.Hosts, ev.On = cfg.AgentEgress(&a)
	}
	s.render(w, "egresspreview", ev)
}

var goIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// stepTokens are the references to a step's results. An ID that is not a Go
// identifier (fetch-data) needs index, or the template fails to parse.
func stepTokens(id string) []string {
	if goIdent.MatchString(id) {
		return []string{"{{.steps." + id + ".output}}", "{{.steps." + id + ".json.KEY}}", "steps." + id + ".exit == 0"}
	}
	q := strconv.Quote(id)
	return []string{"{{index .steps " + q + " \"output\"}}", "{{index .steps " + q + " \"json\" \"KEY\"}}", "steps[" + q + "].exit == 0"}
}
