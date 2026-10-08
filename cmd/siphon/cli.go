package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/olafkfreund/siphon/internal/client"
)

// The CLI has two modes. Local mode is the original code that opens the config
// file and database directly (serve, run-once, validate, ... and anything
// given -config). Client mode talks to a running siphon over its API.

// localOnly never goes through the API.
var localOnly = []string{"version", "exec-job", "agent-run", "mcp-bridge", "schema", "validate", "config", "rules", "run-once", "serve", "credentials", "backup"}

// clientMode reports whether cmd runs against a server: not a local-only
// command, and not one of the old local commands given -config.
func clientMode(cmd string, args []string) bool {
	if slices.Contains(localOnly, cmd) {
		return false
	}
	if cmd == "jobs" || cmd == "approve" || cmd == "deny" {
		for _, a := range args {
			if a == "-config" || a == "--config" || strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config=") {
				return false
			}
		}
	}
	return true
}

// globals are the flags every client command takes.
type globals struct {
	url, tokenFile, output string
	quiet                  bool
}

// register adds the shared flags; a command that uses -url for its own
// purpose (connect, new) leaves the server URL to SIPHON_URL / client.yaml.
func (g *globals) register(fs *flag.FlagSet, ownURL bool) {
	if !ownURL {
		fs.StringVar(&g.url, "url", "", "siphon URL (default: the saved login, else SIPHON_URL; needs -token-file or SIPHON_TOKEN* unless it is the saved login's)")
	}
	fs.StringVar(&g.tokenFile, "token-file", "", "file holding the API token (default: SIPHON_TOKEN_FILE, SIPHON_TOKEN, then client.yaml)")
	fs.BoolVar(&client.InsecureHTTP, "insecure-http", false, "allow http:// to a non-loopback host (default: SIPHON_INSECURE_HTTP=1; the token is then sent in clear text)")
	fs.StringVar(&g.output, "o", "text", "output format: text or json")
	fs.BoolVar(&g.quiet, "quiet", false, "print only data and errors, no progress messages")
}

type flagDoc struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Default     string `json:"default"`
	Description string `json:"description"`
}

// command is one row of the table behind usage, `help` and `help --json`.
// build registers the command's flags on fs and returns what to run; local
// commands have no build and document their flags in extra.
type command struct {
	Name, Summary, Usage, Example, Mode string
	build                               func(fs *flag.FlagSet) func(c *cli, args []string) error
	extra                               []flagDoc
	ownURL                              bool // -url means something else here
	alias                               string
}

func cfgFlag() flagDoc {
	return flagDoc{"config", "string", "siphon.yaml", "config file (runs the command locally, without the API)"}
}

func commands() []command {
	return []command{
		{Name: "login", Usage: "login [url]", Mode: "client", build: buildLogin, Summary: "save the server URL (default SIPHON_URL) and API token (token from a no-echo prompt, or stdin)", Example: "siphon login http://127.0.0.1:8080"},
		{Name: "logout", Usage: "logout", Mode: "client", build: buildLogout, Summary: "forget the saved login", Example: "siphon logout"},
		{Name: "status", Usage: "status", Mode: "client", build: buildStatus, Summary: "sources, rules, pending approvals and recent jobs at a glance", Example: "siphon status -o json"},
		{Name: "get", Usage: "get <kind> [name]", Mode: "client", build: buildGet, Summary: "list or show config items (sources rules agents routines credentials notify) or jobs, approvals, audit, connections", Example: "siphon get rules disk-full"},
		{Name: "apply", Usage: "apply -f <file|-> [--dry-run] [--secret k=@file|k=-] [--yes]", Mode: "client", build: buildApply, Summary: "create or update items from a siphon.yaml-shaped file, all or nothing", Example: "siphon apply -f task.yaml --dry-run"},
		{Name: "edit", Usage: "edit <kind> <name>", Mode: "client", build: buildEdit, Summary: "edit one item in $EDITOR, with a stale-edit check", Example: "siphon edit rules disk-full"},
		{Name: "delete", Usage: "delete <kind> <name> [--dry-run]", Mode: "client", build: buildDelete, Summary: "delete an item (a file item is hidden, a portal item removed)", Example: "siphon delete rules disk-full --dry-run"},
		{Name: "reset", Usage: "reset <kind> <name> [--dry-run]", Mode: "client", build: buildReset, Summary: "drop the portal's change to an item, back to siphon.yaml", Example: "siphon reset rules disk-full"},
		{Name: "enable", Usage: "enable <rule>", Mode: "client", build: buildToggle("enable"), Summary: "turn a rule on (runtime override)", Example: "siphon enable disk-full"},
		{Name: "disable", Usage: "disable <rule>", Mode: "client", build: buildToggle("disable"), Summary: "turn a rule off (runtime override)", Example: "siphon disable disk-full"},
		{Name: "history", Usage: "history [kind name | <rev>]", Mode: "client", build: buildHistory, Summary: "config revisions, newest first; a revision number shows its diff", Example: "siphon history rules disk-full"},
		{Name: "restore", Usage: "restore <rev> [--yes]", Mode: "client", build: buildRestore, Summary: "put the config overlay back as it was at a revision", Example: "siphon restore 12 --yes"},
		{Name: "jobs", Usage: "jobs [ls|show <id>] [-state s]", Mode: "client", build: buildJobs, Summary: "list jobs, or show one (with -config: the local `jobs ls`)", Example: "siphon jobs ls -state pending_approval"},
		{Name: "approve", Usage: "approve <job id>", Mode: "client", build: buildDecide("approve"), Summary: "approve a pending job (with -config: local)", Example: "siphon approve 42"},
		{Name: "deny", Usage: "deny <job id>", Mode: "client", build: buildDecide("deny"), Summary: "deny a pending job (with -config: local)", Example: "siphon deny 42"},
		{Name: "connect", Usage: "connect <service>|model|login [--<field> value ...]", Mode: "client", build: buildConnect, ownURL: true, Summary: "connect a service (see `siphon catalog`) or model: a flag per field, prompts for the rest, secrets only via - or @file", Example: "siphon connect github --name gh --token @token.txt --webhook"},
		{Name: "catalog", Usage: "catalog [--category c]", Mode: "client", build: buildCatalog, Summary: "the services you can connect, by category, with what each needs on this install", Example: "siphon catalog --category code -o json"},
		{Name: "test", Usage: "test <rule> [event.json|-] [--last] [--at <time>] | test service|model <name>", Mode: "client", build: buildTest, Summary: "dry-run a rule on an event, the last stored one, or (schedule rules) the event for a moment with --at; or check a service or model connection", Example: "siphon test disk-full --last"},
		{Name: "why", Usage: "why <rule>", Mode: "client", build: buildWhy, Summary: "why a rule did or didn't fire: a checklist, the likely reason and what to run next", Example: "siphon why disk-full"},
		{Name: "new", Usage: "new task [--name n --source s|--webhook w|--poll p --url u|--schedule at [--timezone Z] ...] [--print]", Mode: "client", build: buildNew, ownURL: true, Summary: "build a rule (and its source) with a wizard or flags, then dry-run, confirm and apply", Example: "siphon new task --name alert --webhook alerts --when 'event.sev == \"high\"' --cmd '[\"notify\"]' --print"},
		{Name: "template", alias: "example", Usage: "template [name]  (alias: example)", Mode: "client", build: buildTemplate, Summary: "list the ready-made task templates, or print one to pipe into `apply -f -`", Example: "siphon template github-pr-review > pr.yaml"},
		{Name: "explain", Usage: "explain source|rule|agent|routine|credential|notify", Mode: "client", build: buildExplain, Summary: "every field of a config kind: type, required, default, allowed values", Example: "siphon explain rule -o json"},
		{Name: "inventory", Usage: "inventory", Mode: "client", build: buildInventory, Summary: "names of everything configured plus the operator's allowlists (never secrets)", Example: "siphon inventory -o json"},
		{Name: "guide", Usage: "guide", Mode: "client", build: buildGuide, Summary: "print the guide for LLM agents (docs/llm.md)", Example: "siphon guide"},
		{Name: "draft", Usage: `draft "<text>" [--connection c] [--model m] [--apply] [--yes]`, Mode: "client", build: buildDraft, Summary: "have a model connection draft an apply file from plain words; checked, never applied without --apply", Example: `siphon draft "tell me on ntfy when a deploy webhook reports failed"`},
		{Name: "mcp", Usage: "mcp [--allow-write] [--allow-secrets] [--allow-unapproved]", Mode: "client", build: buildMCP, Summary: "run an MCP server on stdio so an assistant can inspect and (with --allow-write) change this siphon", Example: "claude mcp add siphon -- siphon mcp"},
		{Name: "notify", Usage: "notify add <name> --type t --url -|@file [--token -|@file] [--events a,b] [--dry-run] [--yes] | test <name> | log [--channel c] [--job id]", Mode: "client", build: buildNotify, ownURL: true, Summary: "notification channels (ntfy, Slack, webhook): add one (secrets only via - or @file), send a test message, or show the delivery log", Example: "printf %s https://ntfy.sh/my-topic | siphon notify add phone --type ntfy --url - --yes"},
		{Name: "help", Usage: "help [command] [--json]", Mode: "client", build: buildHelp, Summary: "usage; --json is the machine-readable command list", Example: "siphon help --json"},

		{Name: "validate", Usage: "validate [-config f] [-v] [-file-only]", Mode: "local", Summary: "check the config (file + portal edits; -file-only: file alone; -v: print egress allowlists)", Example: "siphon validate -config siphon.yaml",
			extra: []flagDoc{cfgFlag(), {"v", "bool", "false", "print egress allowlists"}, {"file-only", "bool", "false", "ignore portal edits"}}},
		{Name: "config export", Usage: "config export [-config f]", Mode: "local", Summary: "print the effective YAML (file + portal edits)", Example: "siphon config export -config siphon.yaml", extra: []flagDoc{cfgFlag()}},
		{Name: "rules test", Usage: "rules test [-config f] <rule> <event>", Mode: "local", Summary: "dry-run a rule against a saved event (JSON file)", Example: "siphon rules test -config siphon.yaml disk-full event.json", extra: []flagDoc{cfgFlag()}},
		{Name: "run-once", Usage: "run-once [-config f]", Mode: "local", Summary: "poll every source once and run matching actions", Example: "siphon run-once -config siphon.yaml", extra: []flagDoc{cfgFlag()}},
		{Name: "serve", Usage: "serve [-config f]", Mode: "local", Summary: "run the daemon", Example: "siphon serve -config siphon.yaml", extra: []flagDoc{cfgFlag()}},
		{Name: "credentials import", Usage: "credentials import [-config f] [-token-stdin] <name>", Mode: "local", Summary: "store a login read from stdin", Example: "siphon credentials import -config siphon.yaml -token-stdin claude-main", extra: []flagDoc{cfgFlag(), {"token-stdin", "bool", "false", "the input is a setup token, not a login file"}}},
		{Name: "credentials ls", Usage: "credentials ls [-config f]", Mode: "local", Summary: "list stored logins (no secrets)", Example: "siphon credentials ls -config siphon.yaml", extra: []flagDoc{cfgFlag()}},
		{Name: "backup create", Usage: "backup create [-config f | -db path] [--force] <file|->", Mode: "local", Summary: "write a tar.gz of the database, secrets and logins (sensitive: mode 0600; encrypt it)", Example: "siphon backup create -config siphon.yaml /var/backup/siphon.tar.gz",
			extra: []flagDoc{cfgFlag(), {"db", "string", "", "database file (wins over -config)"}, {"force", "bool", "false", "replace an existing file"}}},
		{Name: "backup restore", Usage: "backup restore [-config f | -db path] [--yes] <file|->", Mode: "local", Summary: "restore a backup into a stopped siphon's state directory; the old state is kept in pre-restore-*", Example: "siphon backup restore -config siphon.yaml --yes siphon.tar.gz",
			extra: []flagDoc{cfgFlag(), {"db", "string", "", "database file (wins over -config)"}, {"yes", "bool", "false", "replace existing state without asking (required with -)"}}},
		{Name: "schema", Usage: "schema", Mode: "local", Summary: "print the JSON Schema for siphon.yaml", Example: "siphon schema"},
		{Name: "version", Usage: "version", Mode: "local", Summary: "print the version", Example: "siphon version"},
	}
}

// usageText is generated from the table.
func usageText() string {
	var b strings.Builder
	b.WriteString("usage: siphon <command> [flags]\n\ncommands:\n")
	for _, c := range commands() {
		fmt.Fprintf(&b, "  %-50s %s\n", c.Usage, c.Summary)
	}
	b.WriteString("\nflags every server command takes: -url, -token-file, -o text|json, -quiet\nexit codes: 0 ok, 1 error, 2 usage, 3 validation, 4 not found, 5 conflict\n")
	return b.String()
}

func flagType(f *flag.Flag) string {
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "bool"
	}
	switch t := fmt.Sprintf("%T", f.Value); {
	case strings.Contains(t, "int"):
		return "int"
	case strings.Contains(t, "duration"):
		return "duration"
	case strings.Contains(t, "multi"):
		return "string (repeatable)"
	}
	return "string"
}

func flagDocs(fs *flag.FlagSet, skip map[string]bool) []flagDoc {
	out := []flagDoc{}
	fs.VisitAll(func(f *flag.Flag) {
		if !skip[f.Name] {
			out = append(out, flagDoc{f.Name, flagType(f), f.DefValue, f.Usage})
		}
	})
	return out
}

func globalFlagDocs() []flagDoc {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	(&globals{}).register(fs, false)
	return flagDocs(fs, nil)
}

type cmdDoc struct {
	Name    string    `json:"name"`
	Summary string    `json:"summary"`
	Usage   string    `json:"usage"`
	Mode    string    `json:"mode"`
	Flags   []flagDoc `json:"flags"`
	Example string    `json:"example"`
}

type helpDoc struct {
	Version     string            `json:"version"`
	Commands    []cmdDoc          `json:"commands"`
	GlobalFlags []flagDoc         `json:"global_flags"`
	ExitCodes   map[string]string `json:"exit_codes"`
	Kinds       []string          `json:"kinds"`
}

func (c command) doc() cmdDoc {
	d := cmdDoc{c.Name, c.Summary, "siphon " + c.Usage, c.Mode, c.extra, c.Example}
	if c.build != nil {
		fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
		c.build(fs)
		d.Flags = flagDocs(fs, nil)
	}
	if d.Flags == nil {
		d.Flags = []flagDoc{}
	}
	return d
}

func buildHelpDoc() helpDoc {
	h := helpDoc{Version: version, GlobalFlags: globalFlagDocs(), Kinds: append(slices.Clone(configKinds), listKinds...),
		ExitCodes: map[string]string{"0": "ok", "1": "error", "2": "usage", "3": "validation failed", "4": "not found", "5": "conflict (stale edit)"}}
	for _, c := range commands() {
		h.Commands = append(h.Commands, c.doc())
	}
	return h
}

// cli is one client-mode invocation.
type cli struct {
	g          globals
	in         io.Reader
	out, errw  io.Writer
	isTTY      bool // stdin is a terminal: prompts allowed
	rd         *bufio.Reader
	stdinUsed  bool
	mu         sync.Mutex // api() is called from concurrent MCP tool calls
	actor      string     // audit label override (the MCP server)
	cl         *client.Client
	runEditor  func(path string) error // tests replace it
	secretRead func() (string, error)  // no-echo prompt; tests replace it
}

func (c *cli) json() bool { return c.g.output == "json" }

func (c *cli) say(format string, a ...any) {
	if !c.g.quiet && !c.json() {
		fmt.Fprintf(c.out, format, a...)
	}
}

func (c *cli) jsonOut(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *cli) api() (*client.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cl != nil {
		return c.cl, nil
	}
	conn, err := client.Resolve(c.g.url, c.g.tokenFile)
	if err != nil {
		return nil, err
	}
	c.cl = client.New(conn)
	c.cl.Label = c.actor
	return c.cl, nil
}

// call is one API request through the resolved client.
func (c *cli) call(method, path string, body, out any) error {
	cl, err := c.api()
	if err != nil {
		return err
	}
	return cl.Do(method, path, body, out)
}

func (c *cli) table(head []string, rows [][]string) error {
	w := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(head, "\t"))
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	return w.Flush()
}

func (c *cli) readLine() (string, error) {
	if c.rd == nil {
		c.rd = bufio.NewReader(c.in)
	}
	s, err := c.rd.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

var errNeedYes = client.Usage("this needs confirmation and -o json cannot ask", "pass --yes to apply without asking, or --dry-run to only look")

// confirm asks a yes/no question on a terminal; elsewhere it refuses to guess.
func (c *cli) confirm(question string) (bool, error) {
	if !c.isTTY {
		return false, client.Usage("this needs confirmation and there is no terminal", "pass --yes to apply without asking, or --dry-run to only look")
	}
	fmt.Fprintf(c.out, "%s [y/N] ", question)
	s, err := c.readLine()
	if err != nil {
		return false, nil
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "y"), nil
}

// parseArgs is flag parsing that allows flags after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// runClient is client mode: parse, run, report. It returns the exit code.
func runClient(args []string, in io.Reader, out, errw io.Writer, tty bool) int {
	return (&cli{in: in, out: out, errw: errw, isTTY: tty, g: globals{output: "text"}}).run(args)
}

func (c *cli) run(args []string) int {
	errw := c.errw
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		args = []string{"help"}
	}
	if len(args) == 0 {
		fmt.Fprint(errw, usageText())
		return client.ExitUsage
	}
	var cmd *command
	for _, k := range commands() {
		if (k.Name == args[0] || (k.alias != "" && k.alias == args[0])) && k.build != nil {
			cmd = &k
		}
	}
	if cmd == nil {
		return c.report(client.Usage("unknown command "+args[0], "run `siphon help` for the list"))
	}
	fs := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.g.register(fs, cmd.ownURL)
	run := cmd.build(fs)
	pos, err := parseArgs(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		c.helpFor(*cmd)
		return 0
	}
	if err != nil {
		return c.report(client.Usage(err.Error(), "run `siphon help "+cmd.Name+"` for the flags"))
	}
	if c.g.output != "text" && c.g.output != "json" {
		c.g.output = "text"
		return c.report(client.Usage("-o must be text or json", ""))
	}
	if err := run(c, pos); err != nil {
		return c.report(err)
	}
	return 0
}

// report prints err (JSON with -o json) and returns its exit code.
func (c *cli) report(err error) int {
	var ce *client.Error
	if !errors.As(err, &ce) {
		ce = &client.Error{Msg: err.Error()}
	}
	if ce.Errors == nil {
		ce.Errors = []string{}
	}
	if c.json() {
		enc := json.NewEncoder(c.errw)
		enc.SetEscapeHTML(false)
		enc.Encode(ce)
		return ce.ExitCode()
	}
	fmt.Fprintln(c.errw, "siphon:", ce.Msg)
	for _, e := range ce.Errors {
		if e != ce.Msg {
			fmt.Fprintln(c.errw, "  -", e)
		}
	}
	if ce.Hint != "" {
		fmt.Fprintln(c.errw, "hint:", ce.Hint)
	}
	return ce.ExitCode()
}

func (c *cli) helpFor(k command) {
	d := k.doc()
	fmt.Fprintf(c.out, "%s\n\nusage: %s\n\n", d.Summary, d.Usage)
	if len(d.Flags) > 0 {
		tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
		for _, f := range d.Flags {
			fmt.Fprintf(tw, "  -%s\t%s\t%s\n", f.Name, f.Type, f.Description)
		}
		tw.Flush()
	}
	fmt.Fprintf(c.out, "\nexample: %s\n", d.Example)
}

func buildHelp(fs *flag.FlagSet) func(c *cli, args []string) error {
	asJSON := fs.Bool("json", false, "print the command list as JSON")
	return func(c *cli, args []string) error {
		if *asJSON || c.json() {
			c.g.output = "json"
			return c.jsonOut(buildHelpDoc())
		}
		if len(args) > 0 {
			name := strings.Join(args, " ")
			for _, k := range commands() {
				if k.Name == name {
					c.helpFor(k)
					return nil
				}
			}
			return client.Usage("unknown command "+name, "run `siphon help` for the list")
		}
		fmt.Fprint(c.out, usageText())
		return nil
	}
}
