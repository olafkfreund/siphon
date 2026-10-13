package docs_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/docs"
)

func TestTemplatesParse(t *testing.T) {
	ts := docs.Templates()
	if len(ts) < 20 {
		t.Fatalf("%d templates", len(ts))
	}
	rank := func(c string) int { return slices.Index(docs.Categories, c) }
	for i, tpl := range ts {
		if tpl.Title == "" || tpl.Category == "" || !strings.HasPrefix(tpl.Apply, "siphon apply -f "+tpl.Name+".yaml") || tpl.YAML == "" {
			t.Errorf("%s: %+v", tpl.Name, tpl)
		}
		if rank(tpl.Category) < 0 {
			t.Errorf("%s: unknown category %q", tpl.Name, tpl.Category)
		}
		if i > 0 && (rank(ts[i-1].Category) > rank(tpl.Category) || ts[i-1].Category == tpl.Category && ts[i-1].Name >= tpl.Name) {
			t.Errorf("order: %s before %s", ts[i-1].Name, tpl.Name)
		}
		for _, r := range append(slices.Clone(tpl.Needs), tpl.Secrets...) {
			if !strings.Contains(r, "/") || strings.ContainsAny(r, "() ,") {
				t.Errorf("%s: bad ref %q", tpl.Name, r)
			}
		}
	}
	// continuation lines under needs, and secrets, and notes
	a, _ := docs.Lookup("aws-cloudwatch-alarm")
	if !slices.Equal(a.Needs, []string{"sources/aws-cloudwatch", "sources/aws-hooks", "credentials/claude-max"}) || !strings.Contains(a.Notes, "EventBridge") {
		t.Errorf("aws-cloudwatch-alarm: %+v", a)
	}
	m, _ := docs.Lookup("alertmanager-summary")
	if !slices.Equal(m.Secrets, []string{"sources/alertmanager.secret"}) || !strings.Contains(m.Notes, "  receivers:") || strings.Contains(m.Notes, "title:") {
		t.Errorf("alertmanager-summary: %+v", m)
	}
	if _, ok := docs.Lookup("nope"); ok {
		t.Error("found a template that does not exist")
	}
}

func TestPage(t *testing.T) {
	if b, ok := docs.Page("llm"); !ok || len(b) == 0 {
		t.Fatal("llm")
	}
	if _, ok := docs.Page("llm.md"); !ok {
		t.Fatal("llm.md")
	}
	if _, ok := docs.Page("templates/disk-full.yaml"); ok {
		t.Fatal("a template is not a page")
	}
	if _, ok := docs.Page("nope"); ok {
		t.Fatal("nope")
	}
}

// Applying a template must never overwrite what another template created:
// apply replaces an item of the same kind and name in place.
func TestTemplateNamesAreUnique(t *testing.T) {
	owner := map[string]string{}
	for _, tpl := range docs.Templates() {
		var doc struct {
			Agents, Routines, Sources map[string]yaml.Node
			Rules                     []struct{ Name string }
		}
		if err := yaml.Unmarshal([]byte(tpl.YAML), &doc); err != nil {
			t.Fatalf("%s: %v", tpl.Name, err)
		}
		var keys []string
		for kind, m := range map[string]map[string]yaml.Node{"agent": doc.Agents, "routine": doc.Routines, "source": doc.Sources} {
			for n := range m {
				keys = append(keys, kind+" "+n)
			}
		}
		for _, r := range doc.Rules {
			keys = append(keys, "rule "+r.Name)
		}
		for _, k := range keys {
			if o, ok := owner[k]; ok && o != tpl.Name {
				t.Errorf("%s: %s is also in %s", tpl.Name, k, o)
			}
			owner[k] = tpl.Name
		}
	}
}

type tplStep struct {
	ID    string
	Agent string
	Cmd   []string
}

type tplDoc struct {
	Routines map[string]struct{ Steps []tplStep }
	Rules    []struct{ Action struct{ Cmd []string } }
}

// parseTemplate returns a template's document and every rule and routine-step cmd.
func parseTemplate(t *testing.T, tpl docs.Template) (tplDoc, [][]string) {
	t.Helper()
	var doc tplDoc
	if err := yaml.Unmarshal([]byte(tpl.YAML), &doc); err != nil {
		t.Fatalf("%s: %v", tpl.Name, err)
	}
	var cmds [][]string
	for _, r := range doc.Rules {
		cmds = append(cmds, r.Action.Cmd)
	}
	for _, rt := range doc.Routines {
		for _, s := range rt.Steps {
			cmds = append(cmds, s.Cmd)
		}
	}
	return doc, cmds
}

// A guard rejects an argument that renders to a leading dash, so each
// templated argument starts with literal text.
func TestTemplateArgsStartLiteral(t *testing.T) {
	for _, tpl := range docs.Templates() {
		_, cmds := parseTemplate(t, tpl)
		for _, c := range cmds {
			if len(c) == 0 {
				continue
			}
			for _, a := range c[1:] {
				if strings.HasPrefix(strings.TrimSpace(a), "{{") { // " {{- .x}}" trims the space
					t.Errorf("%s: argument %q starts with a template action: the guard rejects a value that renders to a leading dash; put a literal label first", tpl.Name, a)
				}
			}
		}
	}
}

// `-d @x` reads a file, and an agent step's .output is its JSON envelope:
// notifications use --data-raw and .json.result.
func TestTemplatesUseDataRaw(t *testing.T) {
	// curl flags that read a file for a leading @ (or take a form file): any spelling.
	readsFile := regexp.MustCompile(`^(-[a-zA-Z]*[dF]|--data|--json|--form)`)
	envelope := regexp.MustCompile(`\{\{-?\s*\.steps\.([A-Za-z0-9_-]+)\.output\b`)
	for _, tpl := range docs.Templates() {
		doc, cmds := parseTemplate(t, tpl)
		for _, rt := range doc.Routines {
			var agentIDs []string
			for _, s := range rt.Steps {
				if s.Agent != "" {
					agentIDs = append(agentIDs, s.ID)
				}
			}
			for _, s := range rt.Steps {
				for _, a := range s.Cmd {
					for _, m := range envelope.FindAllStringSubmatch(a, -1) {
						if slices.Contains(agentIDs, m[1]) {
							t.Errorf("%s: step %s sends the JSON envelope of %s; use .json.result", tpl.Name, s.ID, m[1])
						}
					}
				}
			}
		}
		for _, c := range cmds {
			if len(c) == 0 || c[0] != "curl" {
				continue
			}
			for _, a := range c[1:] {
				if a != "--data-raw" && readsFile.MatchString(a) {
					t.Errorf("%s: curl %s; use --data-raw", tpl.Name, a)
				}
			}
		}
	}
}
