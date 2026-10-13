package docs_test

import (
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

// `-d @x` reads a file, and an agent step's .output is its JSON envelope:
// notifications use --data-raw and .json.result.
func TestTemplatesUseDataRaw(t *testing.T) {
	bad := []string{"-d", "--data", "--data-binary", "--data-ascii", "--data-urlencode"}
	for _, tpl := range docs.Templates() {
		type step struct {
			ID    string
			Agent string
			Cmd   []string
		}
		var doc struct {
			Routines map[string]struct{ Steps []step }
			Rules    []struct{ Action struct{ Cmd []string } }
		}
		if err := yaml.Unmarshal([]byte(tpl.YAML), &doc); err != nil {
			t.Fatalf("%s: %v", tpl.Name, err)
		}
		var cmds [][]string
		for _, r := range doc.Rules {
			cmds = append(cmds, r.Action.Cmd)
		}
		for _, rt := range doc.Routines {
			var agentIDs []string
			for _, s := range rt.Steps {
				cmds = append(cmds, s.Cmd)
				if s.Agent != "" {
					agentIDs = append(agentIDs, s.ID)
				}
			}
			for _, s := range rt.Steps {
				for _, a := range s.Cmd {
					for _, id := range agentIDs {
						if strings.Contains(a, "{{.steps."+id+".output}}") {
							t.Errorf("%s: step %s sends the JSON envelope of %s; use .json.result", tpl.Name, s.ID, id)
						}
					}
				}
			}
		}
		for _, c := range cmds {
			for _, a := range c {
				if slices.Contains(bad, a) {
					t.Errorf("%s: curl %s; use --data-raw", tpl.Name, a)
				}
			}
		}
	}
}
