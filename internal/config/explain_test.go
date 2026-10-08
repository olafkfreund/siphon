package config

import (
	"encoding/json"
	"testing"
)

// Every schema field of every kind is explained, with a type, enum list and
// description, and every hint is for a field that exists.
func TestExplainCoversSchema(t *testing.T) {
	raw, _ := Schema()
	var root map[string]any
	json.Unmarshal(raw, &root)
	for kind, k := range explainKinds {
		got, fields, err := Explain(kind)
		if err != nil || got != kind || len(fields) == 0 {
			t.Fatalf("%s: %v", kind, err)
		}
		have := map[string]ExplainField{}
		for _, f := range fields {
			have[f.Path] = f
			if f.Description == "" || f.Type == "" || f.Enum == nil {
				t.Errorf("%s.%s: incomplete %+v", kind, f.Path, f)
			}
		}
		node := sub(sub(sub(root, "properties"), k.plural), "additionalProperties")
		if kind == "rule" {
			node = sub(sub(sub(root, "properties"), "rules"), "items")
		}
		for name := range sub(node, "properties") {
			if _, ok := have[name]; !ok {
				t.Errorf("%s: schema field %s is not explained", kind, name)
			}
		}
		for path := range k.hints {
			if _, ok := have[path]; !ok {
				t.Errorf("%s: hint for %s, which is not in the schema", kind, path)
			}
		}
	}
	if _, _, err := Explain("widget"); err == nil {
		t.Error("unknown kind accepted")
	}
	if k, _, _ := Explain("credentials"); k != "credential" {
		t.Errorf("plural: %q", k)
	}
}
