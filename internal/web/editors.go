package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

type optPair struct{ Name, Type string }

type stepRow struct{ ID, Kind, Target, Flags string }

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
			}
		}
	}
	return nil
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
	var rl config.Rule
	if err := yaml.Unmarshal([]byte(y), &rl); err != nil {
		tv.Err = "rule: " + err.Error()
		return tv
	}
	rl.Name = name
	if rl.Source == "" || cfg.Sources[rl.Source] == nil {
		tv.Err = "Pick a source."
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
		if h := strings.TrimSpace(r.PostFormValue("headers_json")); h != "" {
			var in map[string]string
			if err := json.Unmarshal([]byte(h), &in); err != nil {
				tv.Err = "headers: want a JSON object of strings"
				return tv
			}
			headers = map[string]string{}
			for k, v := range in {
				headers[strings.ToLower(k)] = v
			}
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
	}
	return ruleTest(r.Context(), cfg, rl, data, headers)
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
