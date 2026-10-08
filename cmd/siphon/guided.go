package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/olafkfreund/siphon/internal/client"
)

// ask reads one answer on a terminal (prompts go to stderr, so -o json stays
// clean). An empty answer takes def.
func (c *cli) ask(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(c.errw, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(c.errw, "%s: ", label)
	}
	s, err := c.readLine()
	if err != nil {
		return def, nil
	}
	if s = strings.TrimSpace(s); s == "" {
		return def, nil
	}
	return s, nil
}

func (c *cli) askSecret(label string) (string, error) {
	if c.secretRead != nil {
		return c.secretRead()
	}
	fmt.Fprintf(c.errw, "%s (not shown): ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(c.errw)
	return string(b), err
}

// strField is a non-secret option: a flag, else a prompt on a terminal,
// else its default, else it is reported missing.
type strField struct {
	flag, label string
	p           *string
	def         string
	req         bool
}

// secField is a secret option: only `-` (stdin), `@file`, or the no-echo prompt.
type secField struct {
	flag, label string
	p           *string
	req         bool
}

// gather fills every field or returns one usage error listing what is missing.
func (c *cli) gather(strs []strField, secs []secField) error {
	var missing []string
	for _, f := range strs {
		if *f.p != "" {
			continue
		}
		switch {
		case !f.req && f.def == "":
		case c.isTTY:
			v, _ := c.ask(f.label, f.def)
			*f.p = v
		default:
			*f.p = f.def
		}
		if *f.p == "" && f.req {
			missing = append(missing, "--"+f.flag)
		}
	}
	for _, f := range secs {
		v, err := c.secretValue(f)
		if err != nil {
			return err
		}
		if v == "" && f.req {
			missing = append(missing, "--"+f.flag+" - (or @file)")
		}
		*f.p = v
	}
	if len(missing) > 0 {
		return usageErr("missing: "+strings.Join(missing, ", "), "pass them as flags, or run on a terminal to be prompted (secrets: `-` reads stdin, `@file` reads a file)")
	}
	return nil
}

func (c *cli) secretValue(f secField) (string, error) {
	v := *f.p
	switch {
	case v == "-":
		if c.stdinUsed {
			return "", usageErr("stdin can only supply one value", "give the others as @file")
		}
		c.stdinUsed = true
		b, err := readAll(c)
		return strings.TrimSpace(string(b)), err
	case strings.HasPrefix(v, "@"):
		b, err := os.ReadFile(v[1:])
		if err != nil {
			return "", usageErr("cannot read the file for --"+f.flag, "")
		}
		return strings.TrimSpace(string(b)), nil
	case v != "":
		return "", usageErr("refusing a secret on the command line (--"+f.flag+"): it would end up in your shell history and the process list",
			"use --"+f.flag+" - to read it from stdin, --"+f.flag+" @file, or leave the flag out to be prompted")
	case !f.req || !c.isTTY:
		return "", nil
	}
	s, err := c.askSecret(f.label)
	return strings.TrimSpace(s), err
}

// ---------------------------------------------------------------- connect

type connectOpts struct {
	name, token, mode, base, project, region, profile, roleARN, externalID string
	keyID, secretKey, servers, url, apiKey, file, setupToken               string
	webhook, noTest                                                        bool
}

func buildConnect(fs *flag.FlagSet) func(*cli, []string) error {
	o := &connectOpts{}
	fs.StringVar(&o.name, "name", "", "name of the source, credential or connection (default: the service or preset)")
	fs.StringVar(&o.token, "token", "", "github, gitlab: API token as - (stdin) or @file")
	fs.StringVar(&o.mode, "mode", "", "github: remote or local; aws: role or profile")
	fs.BoolVar(&o.webhook, "webhook", false, "github, gitlab, aws: also create a webhook source (its secret is shown once)")
	fs.StringVar(&o.base, "base", "", "gitlab: base URL (default https://gitlab.com)")
	fs.StringVar(&o.project, "project", "", "gitlab: project path, like group/project")
	fs.StringVar(&o.region, "region", "", "aws: region")
	fs.StringVar(&o.profile, "profile", "", "aws profile mode: profile name from server.aws.profiles")
	fs.StringVar(&o.roleARN, "role-arn", "", "aws role mode: role ARN")
	fs.StringVar(&o.externalID, "external-id", "", "aws role mode: external id")
	fs.StringVar(&o.keyID, "access-key-id", "", "aws role mode: base access key id as - or @file (with --secret-access-key)")
	fs.StringVar(&o.secretKey, "secret-access-key", "", "aws role mode: base secret access key as - or @file")
	fs.StringVar(&o.servers, "servers", "", "aws: servers to add, comma separated (cloudwatch, docs; default cloudwatch)")
	fs.StringVar(&o.url, "url", "", "model: endpoint URL (default: the preset's)")
	fs.StringVar(&o.apiKey, "api-key", "", "model, login: API key as - (stdin) or @file")
	fs.StringVar(&o.file, "file", "", "login: a login file (kind login), or - for stdin")
	fs.StringVar(&o.setupToken, "setup-token", "", "login claude: a setup token as - (stdin) or @file (kind token)")
	fs.BoolVar(&o.noTest, "no-test", false, "skip the connection test")
	return func(c *cli, args []string) error {
		if len(args) < 1 {
			return usageErr("usage: siphon connect github|gitlab|aws|model|login [flags]", "for a model: `siphon connect model ollama --url http://host:11434`")
		}
		switch args[0] {
		case "github", "gitlab", "aws":
			if len(args) != 1 {
				return usageErr("usage: siphon connect "+args[0]+" [flags]", "")
			}
			return c.connectService(args[0], o)
		case "model":
			return c.connectModel(args[1:], o)
		case "login":
			return c.connectLogin(args[1:], o)
		}
		return usageErr("unknown thing to connect: "+args[0], "choose github, gitlab, aws, model or login")
	}
}

func (c *cli) connectService(kind string, o *connectOpts) error {
	body := map[string]any{"webhook": o.webhook}
	strs := []strField{{"name", "Name", &o.name, kind, true}}
	var secs []secField
	switch kind {
	case "github":
		strs = append(strs, strField{"mode", "Mode (remote or local)", &o.mode, "remote", false})
		secs = []secField{{"token", "GitHub token", &o.token, true}}
	case "gitlab":
		strs = append(strs, strField{"project", "Project path (group/project)", &o.project, "", true}, strField{"base", "Base URL", &o.base, "https://gitlab.com", false})
		secs = []secField{{"token", "GitLab token", &o.token, true}}
	case "aws":
		if o.mode == "" {
			o.mode = map[bool]string{true: "role", false: "profile"}[o.roleARN != ""]
		}
		strs = append(strs, strField{"region", "Region", &o.region, "", true}, strField{"servers", "Servers (cloudwatch, docs)", &o.servers, "cloudwatch", false})
		if o.mode == "role" {
			strs = append(strs, strField{"role-arn", "Role ARN", &o.roleARN, "", true})
			secs = []secField{{"access-key-id", "Access key id", &o.keyID, false}, {"secret-access-key", "Secret access key", &o.secretKey, false}}
		} else {
			strs = append(strs, strField{"profile", "Profile", &o.profile, "", true})
		}
	}
	if err := c.gather(strs, secs); err != nil {
		return err
	}
	body["name"] = o.name
	testName := o.name
	switch kind {
	case "github":
		body["token"], body["mode"] = o.token, o.mode
	case "gitlab":
		body["token"], body["project"], body["base"] = o.token, o.project, o.base
	case "aws":
		srv := strings.Split(strings.ReplaceAll(o.servers, " ", ""), ",")
		body["region"], body["mode"], body["servers"] = o.region, o.mode, srv
		body["role_arn"], body["external_id"], body["profile"] = o.roleARN, o.externalID, o.profile
		body["access_key_id"], body["secret_access_key"] = o.keyID, o.secretKey
		testName = ""
		if slices.Contains(srv, "cloudwatch") {
			testName = o.name + "-cloudwatch"
		}
	}
	var done map[string]any
	if err := c.call("POST", "/api/services/"+kind, body, &done); err != nil {
		return err
	}
	if !c.json() {
		c.say("connected %s: %q\n", str(done, "service"), str(done, "name"))
		if h := str(done, "hook"); h != "" {
			c.say("webhook source %q\n  URL:    %s\n", h, str(done, "hook_url"))
			if hh := str(done, "hook_header"); hh != "" {
				c.say("  header: %s: <the secret>\n", hh)
			}
			c.say("  secret: %s\n          (shown once, copy it now)\n", str(done, "hook_secret"))
		}
		if e := str(done, "apply_error"); e != "" {
			fmt.Fprintln(c.errw, "warning: saved, but not applied live:", e)
		}
	}
	var test map[string]any
	var terr error
	if !o.noTest && testName != "" {
		terr = c.call("POST", "/api/services/"+url.PathEscape(testName)+"/test", nil, &test)
	}
	return c.finishConnect(done, test, terr, "test", testName)
}

// finishConnect prints the test result (JSON: with the setup reply) and fails
// if the saved connection did not pass.
func (c *cli) finishConnect(done, test map[string]any, terr error, key, name string) error {
	if c.json() {
		if err := c.jsonOut(map[string]any{"connected": done, key: test}); err != nil {
			return err
		}
	}
	if terr != nil {
		return terr
	}
	if test == nil {
		return nil
	}
	if e := str(test, "error"); e != "" {
		return &client.Error{Msg: "saved, but the test failed: " + e, Hint: "fix the cause, then re-run `siphon test service " + name + "`"}
	}
	if !c.json() {
		c.say("test: ok%s\n", testDetail(test))
	}
	return nil
}

func testDetail(t map[string]any) string {
	var p []string
	for _, k := range []string{"user", "arn"} {
		if v := str(t, k); v != "" {
			p = append(p, v)
		}
	}
	if at, err := time.Parse(time.RFC3339, str(t, "expires_at")); err == nil {
		p = append(p, "keys expire "+at.Local().Format("15:04"))
	}
	if n := str(t, "tools"); n != "" && n != "0" {
		p = append(p, n+" tools")
	}
	if v := str(t, "latency"); v != "" {
		p = append(p, v)
	}
	if len(p) == 0 {
		return ""
	}
	return " (" + strings.Join(p, ", ") + ")"
}

// Hosted presets prompt for a key; a generic OpenAI-compatible endpoint is often
// self-hosted without one, so its key is only taken from --api-key.
var presetNeedsKey = []string{"openrouter", "groq", "mistral"}

func (c *cli) connectModel(args []string, o *connectOpts) error {
	if len(args) != 1 {
		return usageErr("usage: siphon connect model <preset> [--name n] [--url u] [--api-key -]", "presets: ollama, lmstudio, openrouter, groq, mistral, openai")
	}
	preset := args[0]
	if err := c.gather([]strField{{"name", "Name", &o.name, preset, true}}, []secField{{"api-key", "API key", &o.apiKey, slices.Contains(presetNeedsKey, preset) && c.isTTY}}); err != nil {
		return err
	}
	var done map[string]any
	if err := c.call("POST", "/api/connections/models", map[string]any{"name": o.name, "preset": preset, "url": o.url, "api_key": o.apiKey}, &done); err != nil {
		return err
	}
	c.say("connected model endpoint %q\n", o.name)
	var test map[string]any
	var terr error
	if !o.noTest {
		terr = c.call("POST", "/api/connections/models/"+url.PathEscape(o.name)+"/test", nil, &test)
	}
	if terr == nil && test != nil && str(test, "error") == "" && !c.json() {
		models, _ := test["models"].([]any)
		var names []string
		for _, m := range models {
			names = append(names, fmt.Sprint(m))
		}
		shown := names[:min(len(names), 10)]
		c.say("test: ok, %d models%s\n", len(names), map[bool]string{true: ": " + strings.Join(shown, ", "), false: ""}[len(names) > 0])
		return nil
	}
	return c.finishConnect(done, test, terr, "test", o.name)
}

func (c *cli) connectLogin(args []string, o *connectOpts) error {
	if len(args) != 1 || !slices.Contains(loginProviders, args[0]) {
		return usageErr("usage: siphon connect login claude|codex|agy (--file f | --setup-token - | --api-key -)", "")
	}
	provider := args[0]
	given := 0
	for _, v := range []string{o.file, o.setupToken, o.apiKey} {
		if v != "" {
			given++
		}
	}
	if given != 1 {
		return usageErr("give exactly one of --file <login.json>, --setup-token - or --api-key -", "a subscription login file, a Claude setup token, or an API key")
	}
	if err := c.gather([]strField{{"name", "Name", &o.name, provider, true}}, nil); err != nil {
		return err
	}
	kind, value := "login", ""
	switch {
	case o.file != "":
		var b []byte
		var err error
		if o.file == "-" {
			b, err = readAll(c)
		} else {
			b, err = os.ReadFile(o.file)
		}
		if err != nil {
			return usageErr("cannot read the login file", "")
		}
		value = string(b)
	case o.setupToken != "":
		kind = "token"
		if err := c.gather(nil, []secField{{"setup-token", "Setup token", &o.setupToken, true}}); err != nil {
			return err
		}
		value = o.setupToken
	default:
		kind = "apikey"
		if err := c.gather(nil, []secField{{"api-key", "API key", &o.apiKey, true}}); err != nil {
			return err
		}
		value = o.apiKey
	}
	if err := c.call("POST", "/api/connections/logins", map[string]any{"name": o.name, "provider": provider, "kind": kind, "value": value}, nil); err != nil {
		return err
	}
	var conns struct {
		Logins []map[string]any `json:"logins"`
	}
	var login map[string]any
	if err := c.call("GET", "/api/connections", nil, &conns); err == nil {
		for _, l := range conns.Logins {
			if str(l, "name") == o.name {
				login = l
			}
		}
	}
	if c.json() {
		return c.jsonOut(map[string]any{"connected": map[string]any{"name": o.name, "provider": provider, "kind": kind}, "status": login})
	}
	c.say("connected %s login %q: status %s, expiry %s\n", provider, o.name, str(login, "status"), str(login, "expiry"))
	return nil
}

// ---------------------------------------------------------------- test

func buildTest(fs *flag.FlagSet) func(*cli, []string) error {
	last := fs.Bool("last", false, "use the last event the source produced")
	var headers multi
	fs.Var(&headers, "header", "an event header as name=value (repeatable)")
	return func(c *cli, args []string) error {
		if len(args) == 2 && (args[0] == "service" || args[0] == "model") {
			return c.testConnection(args[0], args[1])
		}
		if len(args) < 1 || len(args) > 2 || (*last && len(args) == 2) || (!*last && len(args) == 1) {
			return usageErr("usage: siphon test <rule> <event.json|-> | siphon test <rule> --last | siphon test service|model <name>", "an event is a JSON file, or - for stdin; --last uses the stored one")
		}
		body := map[string]any{}
		if *last {
			body["use_last"] = true
		} else {
			var b []byte
			var err error
			if args[1] == "-" {
				b, err = readAll(c)
			} else {
				b, err = os.ReadFile(args[1])
			}
			if err != nil {
				return usageErr("cannot read the event "+args[1], "")
			}
			if !json.Valid(b) {
				return usageErr("the event is not valid JSON", "")
			}
			body["event"] = json.RawMessage(b)
		}
		if len(headers) > 0 {
			h := map[string]string{}
			for _, kv := range headers {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return usageErr("bad --header "+kv, "want name=value")
				}
				h[k] = v
			}
			body["headers"] = h
		}
		var tv struct {
			Error    string           `json:"error"`
			Fires    []map[string]any `json:"fires"`
			Approval bool             `json:"needs_approval"`
		}
		var raw map[string]any
		if err := c.call("POST", "/api/rules/"+url.PathEscape(args[0])+"/test", body, &raw); err != nil {
			return err
		}
		b, _ := json.Marshal(raw)
		json.Unmarshal(b, &tv)
		if tv.Error != "" {
			return &client.Error{Msg: "the rule failed on this event: " + tv.Error, Hint: "check the expressions with `siphon why " + args[0] + "` or `siphon edit rules " + args[0] + "`"}
		}
		if c.json() {
			return c.jsonOut(raw)
		}
		if len(tv.Fires) == 0 {
			fmt.Fprintln(c.out, "no fire: the rule's `when` is false for this event (or it is already latched)")
			return nil
		}
		rows := make([][]string, len(tv.Fires))
		for i, f := range tv.Fires {
			what := str(f, "target")
			if a, ok := f["argv"].([]any); ok {
				var p []string
				for _, x := range a {
					p = append(p, fmt.Sprint(x))
				}
				what = strings.Join(p, " ")
			}
			rows[i] = []string{str(f, "key"), str(f, "kind"), what, str(f, "error")}
		}
		if err := c.table([]string{"KEY", "ACTION", "WOULD RUN", "ERROR"}, rows); err != nil {
			return err
		}
		if tv.Approval {
			fmt.Fprintln(c.out, "needs approval before it runs")
		}
		return nil
	}
}

func (c *cli) testConnection(kind, name string) error {
	path := "/api/services/" + url.PathEscape(name) + "/test"
	if kind == "model" {
		path = "/api/connections/models/" + url.PathEscape(name) + "/test"
	}
	var t map[string]any
	if err := c.call("POST", path, nil, &t); err != nil {
		return err
	}
	if c.json() {
		if err := c.jsonOut(t); err != nil {
			return err
		}
	}
	if e := str(t, "error"); e != "" {
		return &client.Error{Msg: "the " + kind + " " + name + " failed its test: " + e, Hint: "see `siphon get " + map[string]string{"service": "sources", "model": "connections"}[kind] + "` and fix the token or URL"}
	}
	if !c.json() {
		if models, ok := t["models"].([]any); ok {
			c.say("ok: %d models\n", len(models))
		} else {
			c.say("ok%s\n", testDetail(t))
		}
	}
	return nil
}

// ---------------------------------------------------------------- why

func buildWhy(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		if err := needArgs(args, 1, "why <rule>"); err != nil {
			return err
		}
		var x map[string]any
		if err := c.call("GET", "/api/rules/"+url.PathEscape(args[0])+"/explain", nil, &x); err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(x)
		}
		c.renderWhy(args[0], x)
		return nil
	}
}

func sub(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func (c *cli) renderWhy(rule string, x map[string]any) {
	line := func(ok bool, format string, a ...any) {
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		fmt.Fprintf(c.out, " %s %s\n", mark, fmt.Sprintf(format, a...))
	}
	src := sub(x, "source")
	fmt.Fprintf(c.out, "rule %s\n", rule)
	line(x["enabled"] != false, "%s", map[bool]string{true: "enabled", false: "disabled (turned off at runtime)"}[x["enabled"] != false])
	health := str(src, "health")
	line(health != "error" && health != "stale", "source %s (%s) is %s%s", str(src, "name"), str(src, "type"), health, map[bool]string{true: ": " + str(src, "last_error"), false: ""}[str(src, "last_error") != ""])
	line(x["last_event_at"] != nil, "%s", map[bool]string{true: "last event at " + humanTime(str(x, "last_event_at")), false: "the source has not produced an event yet"}[x["last_event_at"] != nil])
	if m, ok := x["last_event_matches"].(bool); ok {
		line(m, "%s", map[bool]string{true: "the last event satisfies the condition", false: "the last event does not satisfy the condition"}[m])
	}
	left := str(x, "cooldown_left")
	switch {
	case x["held_back_by_cooldown"] == true:
		line(false, "the last event was held back by the cooldown")
	case left == "" || left == "0s":
		line(true, "cooldown: none pending")
	default:
		line(true, "cooldown: %s left (the last event was not held back)", left)
	}
	if x["last_fired"] != nil {
		line(true, "last fired %s", humanTime(str(x, "last_fired")))
	}
	if rows, ok := x["edge_state"].([]any); ok && len(rows) > 0 {
		var latched []string
		for _, r := range rows {
			if m, _ := r.(map[string]any); m["last_value"] == true {
				latched = append(latched, str(m, "key"))
			}
		}
		line(len(latched) == 0, "edge state: %s", map[bool]string{true: "nothing latched", false: "already true for " + strings.Join(latched, ", ") + " (fires again after it goes false)"}[len(latched) == 0])
	}
	if e := sub(x, "last_eval_error"); e != nil {
		line(false, "evaluation error: %s", str(e, "message"))
	} else {
		line(true, "no evaluation errors")
	}
	if r := sub(x, "last_reject"); r != nil {
		line(false, "last webhook delivery refused: %s", str(r, "message"))
	} else {
		line(true, "no webhook deliveries refused")
	}
	pending := false
	reasons, _ := x["reasons"].([]any)
	for _, r := range reasons {
		pending = pending || str(r.(map[string]any), "code") == "awaiting_approval"
	}
	line(!pending, "approvals: %s", map[bool]string{true: "nothing waiting", false: "a job is waiting"}[!pending])
	if len(reasons) == 0 {
		fmt.Fprintln(c.out, "\nNothing blocks it: the last event would fire the rule.")
		return
	}
	first := reasons[0].(map[string]any)
	fmt.Fprintf(c.out, "\nLikely reason: %s\n", str(first, "message"))
	if next := whyNext(rule, str(src, "name"), str(first, "code")); next != "" {
		fmt.Fprintf(c.out, "Next: %s\n", next)
	}
}

func whyNext(rule, source, code string) string {
	switch code {
	case "disabled":
		return "siphon enable " + rule
	case "no_events_yet":
		return "send an event to the source, or `siphon test " + rule + " event.json`"
	case "source_error", "webhook_rejected":
		return "siphon get sources " + source
	case "eval_error", "condition_false":
		return "siphon test " + rule + " --last"
	case "edge_already_true", "cooldown", "fired":
		return "siphon get audit --rule " + rule
	case "awaiting_approval":
		return "siphon get approvals"
	}
	return ""
}

var loginProviders = []string{"claude", "codex", "agy"}
