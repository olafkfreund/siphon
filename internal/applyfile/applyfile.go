// Package applyfile reads a siphon.yaml-shaped apply file into items.
package applyfile

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/client"
	"github.com/olafkfreund/siphon/internal/config"
)

var configKinds = []string{"sources", "rules", "agents", "routines", "credentials"}

// Item is one item of an apply request.
type Item struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	YAML string `json:"yaml"`
}

// Parse reads a siphon.yaml-shaped file into items, keeping each item's
// own YAML text (comments included). Fixed sections are refused.
func Parse(b []byte) ([]Item, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, client.Usage("not valid YAML: "+err.Error(), "")
	}
	if doc.Kind == 0 {
		return nil, client.Usage("the file is empty", "")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, client.Usage("the top level must be a mapping with sections like rules: and sources:", "see `siphon example`, or `siphon help apply`")
	}
	body := func(n *yaml.Node) string {
		if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
			return "{}\n"
		}
		out, _ := yaml.Marshal(n)
		return string(out)
	}
	var items []Item
	for i := 0; i+1 < len(root.Content); i += 2 {
		kind, val := root.Content[i].Value, root.Content[i+1]
		if !config.Kinds[kind] {
			return nil, client.Usage("section "+strconv.Quote(kind)+" can't be applied: only "+strings.Join(configKinds, ", ")+" are editable (server, limits and units live in siphon.yaml)", "")
		}
		switch {
		case kind == "rules" && val.Kind == yaml.SequenceNode:
			for _, r := range val.Content {
				name := ""
				var rest []*yaml.Node
				for j := 0; r.Kind == yaml.MappingNode && j+1 < len(r.Content); j += 2 {
					if r.Content[j].Value == "name" {
						name = r.Content[j+1].Value
						continue
					}
					rest = append(rest, r.Content[j], r.Content[j+1])
				}
				if name == "" {
					return nil, client.Usage("a rule in rules: has no name", "every rule needs `name:`")
				}
				items = append(items, Item{"rules", name, body(&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: rest})})
			}
		case val.Kind == yaml.MappingNode && kind != "rules":
			for j := 0; j+1 < len(val.Content); j += 2 {
				items = append(items, Item{kind, val.Content[j].Value, body(val.Content[j+1])})
			}
		default:
			return nil, client.Usage("section "+strconv.Quote(kind)+" has the wrong shape", "rules is a list of {name: ...}; the other sections map name to settings")
		}
	}
	if len(items) == 0 {
		return nil, client.Usage("the file has no items", "")
	}
	return items, nil
}
