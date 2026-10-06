package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Item is one portal edit layered over the config file. YAML is the item body
// (the value under its name); Deleted is a tombstone that removes it.
// Callers map store.ConfigItem to Item (config does not import store).
type Item struct {
	Kind, Name, YAML string
	Deleted          bool
}

type Key struct{ Kind, Name string }

type Provenance string

const (
	FromFile     Provenance = "file"     // only in the file
	FromPortal   Provenance = "portal"   // only in the overlay
	FromOverride Provenance = "override" // in the file, replaced by the overlay
)

// Kinds are the editable top-level sections; server, limits and units never are.
// "rules" is a list keyed by name, the rest are maps.
var Kinds = map[string]bool{"sources": true, "agents": true, "routines": true, "credentials": true, "rules": true}

// Effective applies items to the file's YAML node tree (keeping key order and
// comments) and returns the merged YAML plus the provenance of every item.
// Items that tombstone a file item drop out of the map.
func Effective(file []byte, items []Item) ([]byte, map[Key]Provenance, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	dec := yaml.NewDecoder(bytes.NewReader(file))
	var doc yaml.Node
	if err := dec.Decode(&doc); err == nil {
		if doc.Kind != yaml.DocumentNode || doc.Content[0].Kind != yaml.MappingNode {
			return nil, nil, errors.New("config: top level must be a mapping")
		}
		root = doc.Content[0]
		if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
			return nil, nil, errors.New("parse config: multiple YAML documents are not supported")
		}
	} else if !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("parse config: %w", err)
	}

	prov := map[Key]Provenance{}
	for kind := range Kinds {
		if sec := mapGet(root, kind); sec != nil {
			for _, n := range itemNames(kind, sec) {
				prov[Key{kind, n}] = FromFile
			}
		}
	}
	for _, it := range items {
		if !Kinds[it.Kind] {
			return nil, nil, fmt.Errorf("overlay: kind %q is not editable", it.Kind)
		}
		k := Key{it.Kind, it.Name}
		var body *yaml.Node
		if !it.Deleted {
			var d yaml.Node
			if err := yaml.Unmarshal([]byte(it.YAML), &d); err != nil || d.Kind != yaml.DocumentNode {
				return nil, nil, fmt.Errorf("overlay: %s %q: invalid YAML %v", it.Kind, it.Name, err)
			}
			body = d.Content[0]
		}
		sec := mapGet(root, it.Kind)
		if sec == nil || sec.Kind == yaml.ScalarNode { // absent or `kind:` (null)
			kind := yaml.MappingNode
			tag := "!!map"
			if it.Kind == "rules" {
				kind, tag = yaml.SequenceNode, "!!seq"
			}
			sec = &yaml.Node{Kind: kind, Tag: tag}
			mapSet(root, it.Kind, sec)
		}
		_, known := prov[k]
		if it.Kind == "rules" {
			setRule(sec, it.Name, body)
		} else {
			mapSetOrDel(sec, it.Name, body)
		}
		switch {
		case body == nil:
			delete(prov, k)
		case known && prov[k] != FromPortal:
			prov[k] = FromOverride
		default:
			prov[k] = FromPortal
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), prov, enc.Close()
}

// LoadWithOverlay is Load with items applied to the file first.
func LoadWithOverlay(path string, items []Item) (*Config, map[Key]Provenance, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	eff, prov, err := Effective(b, items)
	if err != nil {
		return nil, nil, err
	}
	c, err := Parse(eff)
	if err != nil {
		return nil, nil, err
	}
	c.resolveDB(path)
	return c, prov, nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// mapSetOrDel sets key to v, or removes it when v is nil.
func mapSetOrDel(m *yaml.Node, key string, v *yaml.Node) {
	if v != nil {
		mapSet(m, key, v)
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func ruleName(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	if v := mapGet(n, "name"); v != nil {
		return v.Value
	}
	return ""
}

// setRule replaces (in place), appends, or (body nil) removes the rule called name.
func setRule(seq *yaml.Node, name string, body *yaml.Node) {
	if body != nil && mapGet(body, "name") == nil {
		mapSet(body, "name", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
	}
	for i, r := range seq.Content {
		if ruleName(r) == name {
			if body == nil {
				seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
			} else {
				seq.Content[i] = body
			}
			return
		}
	}
	if body != nil {
		seq.Content = append(seq.Content, body)
	}
}

func itemNames(kind string, sec *yaml.Node) (names []string) {
	if kind == "rules" {
		for _, r := range sec.Content {
			if n := ruleName(r); n != "" {
				names = append(names, n)
			}
		}
	} else if sec.Kind == yaml.MappingNode {
		for i := 0; i < len(sec.Content); i += 2 {
			names = append(names, sec.Content[i].Value)
		}
	}
	return names
}
