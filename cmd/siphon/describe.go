package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/olafkfreund/siphon/docs"
	"github.com/olafkfreund/siphon/internal/client"
	"github.com/olafkfreund/siphon/internal/config"
)

// ---------------------------------------------------------------- template

func buildTemplate(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		if len(args) > 1 {
			return client.Usage("usage: siphon template [name]", "")
		}
		if len(args) == 0 {
			ts := docs.Templates()
			if c.json() {
				for i := range ts {
					ts[i].YAML = ""
				}
				return c.jsonOut(ts)
			}
			rows := make([][]string, len(ts))
			for i, t := range ts {
				rows[i] = []string{t.Name, t.Category, t.Title}
			}
			return c.table([]string{"NAME", "CATEGORY", "TITLE"}, rows)
		}
		t, ok := docs.Lookup(args[0])
		if !ok {
			return &client.Error{Status: 404, Msg: "no template named " + args[0], Hint: closeTemplates(args[0])}
		}
		if c.json() {
			return c.jsonOut(t)
		}
		_, err := fmt.Fprint(c.out, t.YAML)
		return err
	}
}

// closeTemplates names templates that share a word with name, those sharing the most first.
func closeTemplates(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	var near []string
	hits := map[string]int{}
	for _, t := range docs.Templates() {
		for _, w := range words {
			if len(w) >= 3 && strings.Contains(t.Name, strings.ToLower(w)) {
				hits[t.Name]++
			}
		}
		if hits[t.Name] > 0 {
			near = append(near, t.Name)
		}
	}
	if len(near) == 0 {
		return "list them with `siphon template`"
	}
	sort.SliceStable(near, func(i, j int) bool { return hits[near[i]] > hits[near[j]] })
	return "did you mean: " + strings.Join(near[:min(len(near), 5)], ", ") + "? List them all with `siphon template`"
}

// ---------------------------------------------------------------- explain

func buildExplain(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		if err := needArgs(args, 1, "explain source|rule|agent|routine|credential|notify"); err != nil {
			return err
		}
		kind, fields, err := config.Explain(args[0])
		if err != nil {
			return client.Usage(err.Error(), "")
		}
		if c.json() {
			return c.jsonOut(map[string]any{"kind": kind, "fields": fields})
		}
		rows := make([][]string, len(fields))
		for i, f := range fields {
			req := ""
			if f.Required {
				req = "required"
			}
			extra := f.Description
			if len(f.Enum) > 0 {
				extra += " [" + strings.Join(f.Enum, "|") + "]"
			}
			if f.Default != "" {
				extra += " (default " + f.Default + ")"
			}
			rows[i] = []string{f.Path, f.Type, req, extra}
		}
		return c.table([]string{"FIELD", "TYPE", "", "MEANING"}, rows)
	}
}

// ---------------------------------------------------------------- inventory, guide

func buildInventory(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		var inv map[string]any
		if err := c.call("GET", "/api/inventory", nil, &inv); err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(inv)
		}
		for _, sec := range []string{"sources", "rules", "agents", "routines", "connections", "mcp_packages"} {
			rows, _ := inv[sec].([]any)
			var names []string
			for _, r := range rows {
				m, _ := r.(map[string]any)
				n := str(m, "name")
				for _, k := range []string{"type", "kind", "provider", "status"} {
					if v := str(m, k); v != "" && sec != "rules" && sec != "routines" && sec != "mcp_packages" {
						n += " " + v
						break
					}
				}
				names = append(names, n)
			}
			fmt.Fprintf(c.out, "%s: %s\n", sec, orNone(names))
		}
		srv := sub(inv, "server")
		for _, p := range [][2]string{{"aws", "profiles"}, {"aws", "role_arns"}, {"models", "private_endpoints"}, {"services", "private_endpoints"}} {
			var vals []string
			l, _ := sub(srv, p[0])[p[1]].([]any)
			for _, v := range l {
				vals = append(vals, fmt.Sprint(v))
			}
			fmt.Fprintf(c.out, "server.%s.%s: %s\n", p[0], p[1], orNone(vals))
		}
		return nil
	}
}

func buildGuide(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		b, ok := docs.Page("llm")
		if !ok {
			return &client.Error{Msg: "this build has no guide", Hint: "read docs/llm.md in the repository"}
		}
		_, err := c.out.Write(b)
		return err
	}
}
