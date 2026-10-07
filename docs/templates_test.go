package docs_test

import (
	"slices"
	"strings"
	"testing"

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
