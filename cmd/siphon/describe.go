package main

import (
	"encoding/json"
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
			return usageErr("usage: siphon template [name]", "")
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

// closeTemplates names templates that share a word with name.
func closeTemplates(name string) string {
	var near []string
	for _, t := range docs.Templates() {
		for _, w := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == ' ' }) {
			if len(w) >= 3 && strings.Contains(t.Name, strings.ToLower(w)) {
				near = append(near, t.Name)
				break
			}
		}
	}
	if len(near) == 0 {
		return "list them with `siphon template`"
	}
	return "did you mean: " + strings.Join(near[:min(len(near), 5)], ", ") + "? List them all with `siphon template`"
}

// ---------------------------------------------------------------- explain

// field is one row of `explain`.
type field struct {
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Default     string   `json:"default"`
	Enum        []string `json:"enum"`
	Description string   `json:"description"`
}

// hint is what the schema doesn't say: whether a field is needed, its
// default, its allowed values and what it is for.
type hint struct {
	req       bool
	def, desc string
	enum      []string
}

func e(vals ...string) []string { return vals }

var explainKinds = map[string]struct {
	plural string
	hints  map[string]hint
}{
	"source": {"sources", map[string]hint{
		"type":             {true, "", "what the source is: a polled HTTP URL, an MCP server, or an incoming webhook", e("http", "mcp", "webhook")},
		"url":              {false, "", "http or remote mcp: the URL to read", nil},
		"command":          {false, "", "stdio mcp: the command (only siphon.yaml may set this; use package: instead)", nil},
		"read":             {false, "", "mcp: what to read each poll", nil},
		"read.resource":    {false, "", "mcp: a resource URI to read", nil},
		"read.tool":        {false, "", "mcp: a tool to call", nil},
		"read.args":        {false, "", "mcp: arguments for the tool", nil},
		"poll":             {false, "", "how often to poll (http and mcp with read); not used by webhooks", nil},
		"auth":             {false, "", "http or remote mcp: authentication", nil},
		"auth.bearer":      {false, "", "bearer token: env:NAME or file:/path (the portal and API store it as a file for you)", nil},
		"allow_private":    {false, "false", "allow a private or loopback address (only siphon.yaml, or a listed services.private_endpoints host)", nil},
		"method":           {false, "GET", "http: the request method", e("GET", "POST")},
		"headers":          {false, "", "http: extra request headers; values are env:/file: refs or literals", nil},
		"body":             {false, "", "http POST: the request body", nil},
		"secret":           {false, "", "webhook: the HMAC key or token (env:NAME or file:/path)", nil},
		"signature":        {false, "", "webhook: how deliveries are verified", e("github", "sha256", "token", "standard-webhooks")},
		"signature_header": {false, "", "webhook, signature sha256: the header carrying the signature", nil},
		"token_header":     {false, "", "webhook, signature token: the header carrying the token", nil},
		"timestamp_header": {false, "", "webhook, signature sha256: the header carrying a signed timestamp", nil},
		"id":               {false, "", "webhook: where the delivery id comes from, like header.X-GitHub-Delivery", nil},
		"env":              {false, "", "stdio mcp: the child's environment; values are env:/file: refs", nil},
		"package":          {false, "", "stdio mcp: a name from server.mcp_packages in siphon.yaml", nil},
		"aws":              {false, "", "mcp package: the aws credential whose short-lived keys the daemon injects", nil},
	}},
	"rule": {"rules", map[string]hint{
		"name":               {true, "", "the rule's name (the key in the rules list)", nil},
		"source":             {true, "", "the source whose events this rule reads", nil},
		"when":               {true, "", "the condition: an expression over event, headers, source and item", nil},
		"for_each":           {false, "", "an expression giving a list; the rule runs once per item", nil},
		"id":                 {false, "", "expression identifying an event or item; required with on: each", nil},
		"on":                 {false, "edge", "edge fires when the condition turns true; each fires once per id", e("edge", "each")},
		"repeat":             {false, "", "on: edge: fire again while still true after this long", nil},
		"cooldown":           {false, "", "minimum time between fires; required for agent actions", nil},
		"allow_agent_events": {false, "false", "let this rule react to agent results", nil},
		"action":             {true, "", "what to run: exactly one of cmd, unit, agent or routine", nil},
		"action.cmd":         {false, "", "a command as an argument list; templates like {{.event.x}} are filled in", nil},
		"action.unit":        {false, "", "a systemd unit to start", nil},
		"action.agent":       {false, "", "the name of an agent", nil},
		"action.routine":     {false, "", "the name of a routine", nil},
		"approve":            {false, "false", "wait for a person to approve each job", nil},
		"egress":             {false, "", "restrict a cmd action's network to a host list", nil},
		"egress.enabled":     {false, "false", "turn the restriction on", nil},
		"egress.allow":       {false, "", "host:port entries the action may reach", nil},
	}},
	"agent": {"agents", map[string]hint{
		"kind":           {false, "claude", "which runner: a subscription CLI or a model endpoint", e("claude", "codex", "agy", "model")},
		"credential":     {false, "", "the connection (login or model endpoint) it runs on", nil},
		"model":          {false, "", "kind model: the model id", nil},
		"command":        {false, "", "override the runner's binary path", nil},
		"runner":         {false, "", "deprecated: use kind and command", nil},
		"prompt":         {false, "", "the instructions; templates like {{.event.x}} are filled in", nil},
		"mcp":            {false, "", "names of mcp sources the agent may use", nil},
		"allowed_tools":  {false, "", "tools the agent may call, like mcp__github__get_me", nil},
		"max_turns":      {false, "", "stop after this many turns", nil},
		"max_budget_usd": {false, "", "stop after this much spend (when the endpoint reports cost)", nil},
		"timeout":        {false, "", "stop after this long", nil},
		"approve":        {false, "true", "wait for a person to approve each run", nil},
		"api_key_file":   {false, "", "a file holding an API key for the runner", nil},
		"egress":         {false, "", "network restriction for the agent", nil},
		"egress.enabled": {false, "true", "turn the restriction on (only siphon.yaml may turn it off)", nil},
		"egress.allow":   {false, "", "extra host:port entries the agent may reach", nil},
	}},
	"routine": {"routines", map[string]hint{
		"steps":                     {true, "", "the steps, run in order", nil},
		"steps[].id":                {false, "", "the step's id, used as steps.<id>.output in later steps", nil},
		"steps[].cmd":               {false, "", "a command as an argument list", nil},
		"steps[].unit":              {false, "", "a systemd unit to start", nil},
		"steps[].agent":             {false, "", "the name of an agent", nil},
		"steps[].if":                {false, "", "an expression; the step runs only if it is true", nil},
		"steps[].retry":             {false, "", "retry a failed step", nil},
		"steps[].retry.attempts":    {false, "1", "total tries (0 or 1 means no retry; at most 10)", nil},
		"steps[].retry.base":        {false, "10s", "first wait between tries", nil},
		"steps[].retry.factor":      {false, "2", "how much each wait grows", nil},
		"steps[].timeout":           {false, "", "stop the step after this long", nil},
		"steps[].continue_on_error": {false, "false", "go on to the next step if this one fails", nil},
		"steps[].approve":           {false, "false", "wait for a person to approve this step", nil},
	}},
	"credential": {"credentials", map[string]hint{
		"provider":          {true, "", "what the connection is for", e("claude", "codex", "agy", "ollama", "openai", "aws")},
		"url":               {false, "", "ollama, openai: the endpoint URL", nil},
		"preset":            {false, "", "openai: which tile the portal shows (display only)", nil},
		"api_key":           {false, "", "an API key: env:NAME or file:/path; without it, claude/codex/agy use a stored subscription login", nil},
		"concurrency":       {false, "1", "parallel jobs on this login", nil},
		"region":            {false, "", "aws: the region", nil},
		"profile":           {false, "", "aws: a profile from server.aws.profiles", nil},
		"role_arn":          {false, "", "aws: a role to assume (listed in server.aws.role_arns, or give it its own keys)", nil},
		"external_id":       {false, "", "aws: the role's external id", nil},
		"access_key_id":     {false, "", "aws role mode: base access key id (env:/file:)", nil},
		"secret_access_key": {false, "", "aws role mode: base secret access key (env:/file:)", nil},
	}},
}

// explainFields is the schema of kind flattened to dotted paths, with hints.
func explainFields(kind string) (string, []field, error) {
	k, ok := explainKinds[kind]
	if !ok {
		for n, v := range explainKinds {
			if v.plural == kind {
				kind, k, ok = n, v, true
			}
		}
	}
	if !ok {
		return "", nil, usageErr("unknown kind "+kind, "kinds: source, rule, agent, routine, credential")
	}
	raw, err := config.Schema()
	if err != nil {
		return "", nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", nil, err
	}
	node := sub(sub(root, "properties"), k.plural)
	if kind == "rule" {
		node = sub(node, "items")
	} else {
		node = sub(node, "additionalProperties")
	}
	var out []field
	walk(node, "", k.hints, &out)
	return kind, out, nil
}

func walk(node map[string]any, prefix string, hints map[string]hint, out *[]field) {
	props := sub(node, "properties")
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := sub(props, k)
		path := prefix + k
		h := hints[path]
		f := field{Path: path, Type: schemaType(p), Required: h.req, Default: h.def, Enum: emptyIfNil(h.enum), Description: h.desc}
		if f.Description == "" {
			f.Description = str(p, "description")
		}
		*out = append(*out, f)
		switch {
		case p["type"] == "object" && p["properties"] != nil:
			walk(p, path+".", hints, out)
		case p["type"] == "array" && sub(p, "items")["properties"] != nil:
			walk(sub(p, "items"), path+"[].", hints, out)
		}
	}
}

func schemaType(p map[string]any) string {
	switch p["type"] {
	case "object":
		if ap := sub(p, "additionalProperties"); ap != nil && p["properties"] == nil {
			return "map of " + schemaType(ap)
		}
		return "object"
	case "array":
		return "list of " + schemaType(sub(p, "items"))
	case "string":
		switch pat := str(p, "pattern"); {
		case strings.HasPrefix(pat, "^[-+]?(0|"):
			return "duration"
		case strings.HasPrefix(str(p, "description"), "env:NAME"):
			return "secret"
		}
		return "string"
	case nil:
		if p["oneOf"] != nil {
			return "bytes"
		}
		return "any"
	}
	return fmt.Sprint(p["type"])
}

func buildExplain(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		if err := needArgs(args, 1, "explain source|rule|agent|routine|credential"); err != nil {
			return err
		}
		kind, fields, err := explainFields(args[0])
		if err != nil {
			return err
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

func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
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
