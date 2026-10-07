package web

import (
	"net/http"
	"sort"

	"github.com/olafkfreund/siphon/internal/store"
)

// inventory is the names of everything configured and the operator's
// allowlists: what an agent needs to write a valid task. Never a secret value.
func (s *server) inventory() map[string]any {
	cfg := s.Config()
	keys := func(n int, add func(add func(name string))) []string {
		out := make([]string, 0, n)
		add(func(name string) { out = append(out, name) })
		sort.Strings(out)
		return out
	}
	type m = map[string]any
	var sources, rules, agents, routines, conns, pkgs []m
	for _, n := range keys(len(cfg.Sources), func(add func(string)) {
		for n := range cfg.Sources {
			add(n)
		}
	}) {
		src := m{"name": n, "type": cfg.Sources[n].Type}
		if sg := cfg.Sources[n].Signature; sg != "" { // how a webhook is verified: tells GitHub's from a generic one
			src["signature"] = sg
		}
		if th := cfg.Sources[n].TokenHeader; th != "" {
			src["token_header"] = th
		}
		sources = append(sources, src)
	}
	off, _ := store.RuleOverrides(s.Store.DB)
	for _, r := range cfg.Rules {
		rules = append(rules, m{"name": r.Name, "source": r.Source, "enabled": !off[r.Name]})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i]["name"].(string) < rules[j]["name"].(string) })
	for _, n := range keys(len(cfg.Agents), func(add func(string)) {
		for n := range cfg.Agents {
			add(n)
		}
	}) {
		a := cfg.Agents[n]
		kind := a.Kind
		if kind == "" {
			kind = "claude"
		}
		agents = append(agents, m{"name": n, "kind": kind, "credential": a.Credential})
	}
	for _, n := range keys(len(cfg.Routines), func(add func(string)) {
		for n := range cfg.Routines {
			add(n)
		}
	}) {
		routines = append(routines, m{"name": n, "steps": len(cfg.Routines[n].Steps)})
	}
	status := map[string]string{}
	for _, l := range s.logins() {
		status[l.Name] = l.Status
	}
	for _, n := range keys(len(cfg.Credentials), func(add func(string)) {
		for n := range cfg.Credentials {
			add(n)
		}
	}) {
		c := cfg.Credentials[n]
		kind, st := "login", status[n]
		switch c.Provider {
		case "ollama", "openai":
			kind, st = "model", "configured"
		case "aws":
			kind, st = "aws", "configured"
		}
		conns = append(conns, m{"name": n, "provider": c.Provider, "kind": kind, "status": st})
	}
	for _, n := range keys(len(cfg.Server.MCPPackages), func(add func(string)) {
		for n := range cfg.Server.MCPPackages {
			add(n)
		}
	}) {
		pkgs = append(pkgs, m{"name": n, "env": nonNilStrings(cfg.Server.MCPPackages[n].Env)})
	}
	return m{"sources": nonNilList(sources), "rules": nonNilList(rules), "agents": nonNilList(agents), "routines": nonNilList(routines),
		"connections": nonNilList(conns), "mcp_packages": nonNilList(pkgs),
		"server": m{
			"aws":      m{"profiles": nonNilStrings(cfg.Server.AWS.Profiles), "role_arns": nonNilStrings(cfg.Server.AWS.RoleARNs)},
			"models":   m{"private_endpoints": nonNilStrings(cfg.Server.Models.PrivateEndpoints)},
			"services": m{"private_endpoints": nonNilStrings(cfg.Server.Services.PrivateEndpoints)},
		}}
}

func nonNilList(l []map[string]any) []map[string]any {
	if l == nil {
		return []map[string]any{}
	}
	return l
}

func nonNilStrings(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (s *server) inventoryAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/inventory", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, func(*http.Request) (any, int, error) { return s.inventory(), 200, nil })
	}))
}
