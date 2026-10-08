package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/client"
)

// taskSpec is everything `new task` decides; the wizard and the flags both
// fill it, and one function turns it into YAML.
type taskSpec struct {
	Name, Source                   string
	NewSource                      *srcSpec
	When, On, ID, Repeat, Cooldown string
	Cmd                            []string
	Agent, Routine                 string
	NewAgent                       *agentSpec
}

type srcSpec struct{ Name, Type, URL, Every, Tool, At, Timezone string }

type agentSpec struct {
	Name, Kind, Credential, Model, Prompt string
	Tools                                 []string
}

func sc(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

// mp is a mapping from alternating keys and values (string or *yaml.Node); empty strings are left out.
func mp(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		var v *yaml.Node
		switch x := kv[i+1].(type) {
		case string:
			if x == "" {
				continue
			}
			v = sc(x)
		case *yaml.Node:
			v = x
		}
		n.Content = append(n.Content, sc(kv[i].(string)), v)
	}
	return n
}

func flowSeq(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, i := range items {
		n.Content = append(n.Content, sc(i))
	}
	return n
}

// yaml is the apply-YAML: new source and agent first, then the rule.
func (t taskSpec) yaml() (string, error) {
	root := mp()
	add := func(k string, v *yaml.Node) { root.Content = append(root.Content, sc(k), v) }
	if s := t.NewSource; s != nil {
		var body *yaml.Node
		switch s.Type {
		case "webhook":
			body = mp("type", "webhook", "signature", "token", "token_header", "X-Siphon-Key")
		case "schedule":
			body = mp("type", "schedule", "at", s.At, "timezone", s.Timezone)
		case "http":
			body = mp("type", "http", "url", s.URL, "poll", s.Every)
		case "mcp":
			body = mp("type", "mcp", "url", s.URL, "read", mp("tool", s.Tool), "poll", s.Every)
		}
		add("sources", mp(s.Name, body))
	}
	if a := t.NewAgent; a != nil {
		body := mp("kind", a.Kind, "credential", a.Credential, "model", a.Model, "prompt", a.Prompt)
		if len(a.Tools) > 0 {
			body.Content = append(body.Content, sc("allowed_tools"), flowSeq(a.Tools))
		}
		add("agents", mp(a.Name, body))
	}
	src := t.Source
	if t.NewSource != nil {
		src = t.NewSource.Name
	}
	action := mp()
	switch {
	case len(t.Cmd) > 0:
		action = mp("cmd", flowSeq(t.Cmd))
	case t.NewAgent != nil:
		action = mp("agent", t.NewAgent.Name)
	case t.Agent != "":
		action = mp("agent", t.Agent)
	case t.Routine != "":
		action = mp("routine", t.Routine)
	}
	rule := mp("name", t.Name, "source", src, "when", t.When, "on", t.On, "id", t.ID, "repeat", t.Repeat, "cooldown", t.Cooldown, "action", action)
	add("rules", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{rule}})
	b, err := yaml.Marshal(root)
	return string(b), err
}

func buildNew(fs *flag.FlagSet) func(*cli, []string) error {
	var t taskSpec
	var webhook, poll, cmdJSON, every, srcURL, schedule, timezone string
	fs.StringVar(&t.Name, "name", "", "the rule's name")
	fs.StringVar(&t.Source, "source", "", "use this existing source")
	fs.StringVar(&webhook, "webhook", "", "create a webhook source with this name (a secret is generated and shown once)")
	fs.StringVar(&poll, "poll", "", "create an http polling source with this name (needs --url)")
	fs.StringVar(&srcURL, "url", "", "the polled URL, with --poll")
	fs.StringVar(&schedule, "schedule", "", `create a schedule source with this "at": a cron line, @daily, or "every 15m"; the source takes the task's name`)
	fs.StringVar(&timezone, "timezone", "", "with --schedule: an IANA zone like Europe/London (default: the server's local zone)")
	fs.StringVar(&every, "every", "5m", "poll interval, with --poll")
	fs.StringVar(&t.When, "when", "", "the condition, an expression over event, headers and item")
	fs.StringVar(&t.On, "on", "", "each (once per --id) or edge (when the condition turns true; the default)")
	fs.StringVar(&t.ID, "id", "", "with --on each: the expression that identifies an event")
	fs.StringVar(&t.Repeat, "repeat", "", "with --on edge: fire again while true after this long")
	fs.StringVar(&cmdJSON, "cmd", "", `run a command: argv as a JSON array, like '["notify","{{.event.msg}}"]'`)
	fs.StringVar(&t.Agent, "agent", "", "run this existing agent")
	fs.StringVar(&t.Routine, "routine", "", "run this existing routine")
	fs.StringVar(&t.Cooldown, "cooldown", "", "minimum time between fires (required for agents)")
	printOnly := fs.Bool("print", false, "only print the YAML, change nothing (pipe it to `siphon apply -f -`)")
	dry := fs.Bool("dry-run", false, "check and show the diff, change nothing")
	yes := fs.Bool("yes", false, "apply without asking")
	return func(c *cli, args []string) error {
		if len(args) != 1 || args[0] != "task" {
			return usageErr("usage: siphon new task [flags]", "run `siphon help new` for the flags")
		}
		flagOnly := t.Name != "" || t.Source != "" || webhook != "" || poll != "" || schedule != "" || t.When != "" || cmdJSON != "" || t.Agent != "" || t.Routine != ""
		if !flagOnly && !c.isTTY {
			return usageErr("no terminal for the wizard, and no flags given", "needs --name, one of --source/--webhook/--poll, --when and one of --cmd/--agent/--routine; or run on a terminal")
		}
		if flagOnly {
			if err := specFromFlags(&t, webhook, poll, srcURL, every, cmdJSON, schedule, timezone); err != nil {
				return err
			}
		} else if err := c.wizard(&t); err != nil {
			return err
		}
		y, err := t.yaml()
		if err != nil {
			return err
		}
		if *printOnly {
			fmt.Fprint(c.out, y)
			if t.NewSource != nil && t.NewSource.Type == "webhook" {
				fmt.Fprintf(c.errw, "note: the webhook source %q needs a secret. Apply with --secret sources/%s.secret=@file, or let `siphon new task` generate one.\n", t.NewSource.Name, t.NewSource.Name)
			}
			return nil
		}
		if err := c.taskFree(t); err != nil {
			return err
		}
		items, err := parseApply([]byte(y))
		if err != nil {
			return err
		}
		sec, secret := map[string]string{}, ""
		if s := t.NewSource; s != nil && s.Type == "webhook" {
			b := make([]byte, 32)
			rand.Read(b)
			secret = hex.EncodeToString(b)
			sec["sources/"+s.Name+".secret"] = secret
		}
		out, err := c.runApply(items, sec, *dry, *yes, false)
		if err != nil {
			return err
		}
		var hook map[string]string
		if secret != "" && out.Applied {
			cl, err := c.api()
			if err != nil {
				return err
			}
			hook = map[string]string{"source": t.NewSource.Name, "url": cl.URL + "/hook/" + t.NewSource.Name, "header": "X-Siphon-Key", "secret": secret}
			c.say("webhook source %q\n  URL:    %s\n  header: X-Siphon-Key: <the secret>\n  secret: %s\n          (shown once, copy it now)\n", hook["source"], hook["url"], secret)
		}
		if c.json() {
			b, _ := json.Marshal(out)
			var m map[string]any
			json.Unmarshal(b, &m)
			if hook != nil {
				m["webhook"] = hook
			}
			return c.jsonOut(m)
		}
		return nil
	}
}

// specFromFlags validates the flag-only form and fills in the defaults.
func specFromFlags(t *taskSpec, webhook, poll, srcURL, every, cmdJSON, schedule, timezone string) error {
	var missing []string
	if t.Name == "" {
		missing = append(missing, "--name")
	}
	switch n := btoi(t.Source != "") + btoi(webhook != "") + btoi(poll != "") + btoi(schedule != ""); {
	case n == 0:
		missing = append(missing, "--source (or --webhook <name>, or --poll <name> --url <u>, or --schedule <at>)")
	case n > 1:
		return usageErr("give only one of --source, --webhook, --poll and --schedule", "")
	}
	if timezone != "" && schedule == "" {
		return usageErr("--timezone is for --schedule", "")
	}
	if schedule != "" { // every moment is a new event: fire on each, with no condition
		if t.When == "" {
			t.When = "true"
		}
		if t.On == "" {
			t.On, t.ID = "each", "event.scheduled_at"
		}
	}
	if poll != "" && srcURL == "" {
		missing = append(missing, "--url")
	}
	if t.When == "" {
		missing = append(missing, "--when")
	}
	switch n := btoi(cmdJSON != "") + btoi(t.Agent != "") + btoi(t.Routine != ""); {
	case n == 0:
		missing = append(missing, "--cmd (or --agent, or --routine)")
	case n > 1:
		return usageErr("give only one of --cmd, --agent and --routine", "")
	}
	if t.Agent != "" && t.Cooldown == "" {
		missing = append(missing, "--cooldown (required for agents)")
	}
	if len(missing) > 0 {
		return usageErr("missing: "+strings.Join(missing, ", "), "run `siphon help new` for the flags, or run on a terminal for the wizard")
	}
	switch t.On {
	case "":
		t.On = "edge"
	case "each", "edge":
	default:
		return usageErr("--on must be each or edge", "")
	}
	if t.On == "each" && t.ID == "" {
		return usageErr("--on each needs --id", "the expression that identifies an event, like --id event.id")
	}
	if t.On == "edge" && t.ID != "" {
		return usageErr("--id is for --on each", "")
	}
	if t.On == "each" && t.Repeat != "" {
		return usageErr("--repeat is for --on edge", "")
	}
	if cmdJSON != "" {
		if err := json.Unmarshal([]byte(cmdJSON), &t.Cmd); err != nil || len(t.Cmd) == 0 {
			return usageErr("--cmd must be a non-empty JSON array of strings", `for example --cmd '["notify","disk full"]'`)
		}
	}
	switch {
	case webhook != "":
		t.NewSource = &srcSpec{Name: webhook, Type: "webhook"}
	case poll != "":
		t.NewSource = &srcSpec{Name: poll, Type: "http", URL: srcURL, Every: every}
	case schedule != "":
		t.NewSource = &srcSpec{Name: t.Name, Type: "schedule", At: schedule, Timezone: timezone}
	}
	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// taskFree refuses names that would silently replace something that exists.
func (c *cli) taskFree(t taskSpec) error {
	exists := func(kind, name string) (bool, error) {
		err := c.call("GET", "/api/config/"+kind+"/"+url.PathEscape(name), nil, nil)
		if ce, ok := err.(*client.Error); ok && ce.Status == 404 {
			return false, nil
		}
		return err == nil, err
	}
	for _, p := range []struct{ kind, name string }{{"rules", t.Name}, {"sources", func() string {
		if t.NewSource != nil {
			return t.NewSource.Name
		}
		return ""
	}()}, {"agents", func() string {
		if t.NewAgent != nil {
			return t.NewAgent.Name
		}
		return ""
	}()}} {
		if p.name == "" {
			continue
		}
		found, err := exists(p.kind, p.name)
		if err != nil {
			return err
		}
		if found {
			return usageErr(p.kind+"/"+p.name+" already exists, and `new task` would replace it", "pick another name, or change it with `siphon edit "+p.kind+" "+p.name+"`")
		}
	}
	return nil
}

// ------------------------------------------------------------ the wizard

func (c *cli) need(label, def string) (string, error) {
	for i := 0; i < 3; i++ {
		v, _ := c.ask(label, def)
		if v != "" {
			return v, nil
		}
	}
	return "", usageErr("no answer for: "+label, "")
}

func names(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		out = append(out, str(r, "name"))
	}
	return out
}

func (c *cli) list(path string) []map[string]any {
	var rows []map[string]any
	c.call("GET", path, nil, &rows)
	return rows
}

var starterWhen = map[string]string{"webhook": `event.action == "opened"`, "http": "event.value > 90", "mcp": "event.error != nil"}

func (c *cli) wizard(t *taskSpec) error {
	say := func(f string, a ...any) { fmt.Fprintf(c.errw, f, a...) }
	var err error
	if t.Name, err = c.need("Name for this task", ""); err != nil {
		return err
	}
	srcs := c.list("/api/sources")
	typeOf := map[string]string{}
	for _, s := range srcs {
		typeOf[str(s, "name")] = str(s, "type")
	}
	say("Existing sources: %s\n", orNone(names(srcs)))
	def := "new"
	if len(srcs) > 0 {
		def = str(srcs[0], "name")
	}
	ans, err := c.need("Source (an existing name, or 'new')", def)
	if err != nil {
		return err
	}
	typ := typeOf[ans]
	if _, ok := typeOf[ans]; ok {
		t.Source = ans
	} else {
		s := &srcSpec{}
		if s.Type, err = c.need("New source type (webhook, http, mcp, or schedule = on a schedule)", "webhook"); err != nil {
			return err
		}
		if !slices.Contains([]string{"webhook", "http", "mcp", "schedule"}, s.Type) {
			return usageErr("the source type must be webhook, http, mcp or schedule", "")
		}
		if ans != "new" {
			s.Name = ans
		} else if s.Name, err = c.need("New source name", ""); err != nil {
			return err
		}
		if s.Type == "schedule" {
			if s.At, err = c.need(`When (cron like "0 9 * * 1-5", @daily, or "every 15m")`, "0 9 * * 1-5"); err != nil {
				return err
			}
			s.Timezone, _ = c.ask("Time zone (empty: the server's local zone)", "")
		} else if s.Type != "webhook" {
			if s.URL, err = c.need("URL", ""); err != nil {
				return err
			}
			if s.Type == "mcp" {
				if s.Tool, err = c.need("Tool to read", ""); err != nil {
					return err
				}
			}
			s.Every, _ = c.ask("Poll every", "5m")
		}
		t.NewSource, typ = s, s.Type
	}
	if typ == "schedule" { // every moment is its own event
		t.When, t.On, t.ID = "true", "each", "event.scheduled_at"
	} else {
		if t.When, err = c.need("Fire when (expression)", starterWhen[typ]); err != nil {
			return err
		}
		if t.On, err = c.need("Fire on each (once per event id) or edge (when the condition turns true)", "edge"); err != nil {
			return err
		}
		if t.On == "each" {
			if t.ID, err = c.need("Event id expression", "event.id"); err != nil {
				return err
			}
		} else {
			t.Repeat, _ = c.ask("Repeat while still true after (empty: never)", "")
		}
	}
	kind, err := c.need("Action: cmd, agent, new-agent or routine", "cmd")
	if err != nil {
		return err
	}
	switch kind {
	case "cmd":
		line, err := c.need(`Command (words, or a JSON array like ["notify","hi"])`, "")
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, "[") {
			if err := json.Unmarshal([]byte(line), &t.Cmd); err != nil {
				return usageErr("that is not a JSON array of strings", "")
			}
		} else {
			t.Cmd = strings.Fields(line)
		}
	case "agent":
		say("Existing agents: %s\n", orNone(names(c.list("/api/config/agents"))))
		if t.Agent, err = c.need("Agent name", ""); err != nil {
			return err
		}
	case "routine":
		say("Existing routines: %s\n", orNone(names(c.list("/api/config/routines"))))
		if t.Routine, err = c.need("Routine name", ""); err != nil {
			return err
		}
	case "new-agent":
		if t.NewAgent, err = c.wizardAgent(); err != nil {
			return err
		}
	default:
		return usageErr("the action must be cmd, agent, new-agent or routine", "")
	}
	def = ""
	if t.Agent != "" || t.NewAgent != nil {
		def = "10m"
	}
	t.Cooldown, _ = c.ask("Cooldown between fires (required for agents)", def)
	if (t.Agent != "" || t.NewAgent != nil) && t.Cooldown == "" {
		return usageErr("agents need a cooldown", "")
	}
	return nil
}

func (c *cli) wizardAgent() (*agentSpec, error) {
	var conns struct {
		Logins []map[string]any `json:"logins"`
		Models []map[string]any `json:"models"`
	}
	c.call("GET", "/api/connections", nil, &conns)
	kindOf := map[string]string{}
	var all []string
	for _, l := range conns.Logins {
		kindOf[str(l, "name")] = str(l, "provider")
		all = append(all, str(l, "name"))
	}
	for _, m := range conns.Models {
		kindOf[str(m, "name")] = "model"
		all = append(all, str(m, "name"))
	}
	a := &agentSpec{}
	var err error
	if a.Name, err = c.need("New agent name", ""); err != nil {
		return nil, err
	}
	fmt.Fprintf(c.errw, "Connections: %s\n", orNone(all))
	if a.Credential, err = c.need("Connection to run it on", ""); err != nil {
		return nil, err
	}
	if a.Kind = kindOf[a.Credential]; a.Kind == "" {
		return nil, usageErr("no connection named "+a.Credential, "connect one first: `siphon connect login ...` or `siphon connect model ...`")
	}
	if a.Kind == "model" {
		if a.Model, err = c.need("Model id", ""); err != nil {
			return nil, err
		}
	}
	if a.Prompt, err = c.need("Prompt (what should it do?)", ""); err != nil {
		return nil, err
	}
	tools, _ := c.ask("Allowed tools, comma separated (empty: none listed)", "")
	for _, x := range strings.Split(tools, ",") {
		if x = strings.TrimSpace(x); x != "" {
			a.Tools = append(a.Tools, x)
		}
	}
	return a, nil
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
