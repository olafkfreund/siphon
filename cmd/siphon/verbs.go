package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/olafkfreund/siphon/internal/applyfile"
	"github.com/olafkfreund/siphon/internal/client"
)

// Config kinds the API edits, and the read-only listings `get` also takes.
var (
	configKinds = []string{"sources", "rules", "agents", "routines", "credentials"}
	listKinds   = []string{"jobs", "approvals", "audit", "connections"}
)

func usageErr(msg, hint string) error { return client.Usage(msg, hint) }

func needArgs(args []string, n int, usage string) error {
	if len(args) != n {
		return usageErr("usage: siphon "+usage, "run `siphon help` for the details")
	}
	return nil
}

func checkKind(kind string) error {
	if !slices.Contains(configKinds, kind) {
		return usageErr("unknown kind "+strconv.Quote(kind), "kinds: "+strings.Join(configKinds, ", "))
	}
	return nil
}

func str(m map[string]any, k string) string {
	if v, ok := m[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func buildLogin(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		raw := os.Getenv("SIPHON_URL") // the default URL, as set for every user by the NixOS module
		if len(args) > 1 || len(args) == 0 && raw == "" {
			return usageErr("usage: siphon login [url]", "give the URL, or set SIPHON_URL")
		} else if len(args) == 1 {
			raw = args[0]
		}
		u, err := client.CleanURL(raw)
		if err != nil {
			return err
		}
		var token string
		switch {
		case c.secretRead != nil:
			token, err = c.secretRead()
		case c.isTTY:
			fmt.Fprint(c.errw, "API token (not shown): ")
			var b []byte
			b, err = term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(c.errw)
			token = string(b)
		default:
			token, err = c.readLine()
		}
		if token = strings.TrimSpace(token); err != nil || token == "" {
			return usageErr("no token given", "pipe it in (`siphon login <url> < token-file`) or type it at the prompt")
		}
		conn := client.Conn{URL: u, Token: token}
		if err := client.New(conn).Do("GET", "/api/rules", nil, nil); err != nil {
			return err
		}
		if err := client.Save(conn); err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(map[string]any{"ok": true, "url": u, "saved": client.Path()})
		}
		c.say("logged in to %s (saved to %s)\n", u, client.Path())
		return nil
	}
}

func buildLogout(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		if err := client.Remove(); err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(map[string]bool{"ok": true})
		}
		c.say("logged out\n")
		return nil
	}
}

type callFn func(method, path string, body, out any) error

type statusData struct {
	sources, rules, approvals, jobs []map[string]any
	states                          map[string]int
	disabled                        []string
}

func fetchStatus(call callFn) (*statusData, error) {
	d := &statusData{states: map[string]int{}}
	for path, out := range map[string]*[]map[string]any{"/api/sources": &d.sources, "/api/rules": &d.rules, "/api/approvals": &d.approvals, "/api/jobs?limit=100": &d.jobs} {
		if err := call("GET", path, nil, out); err != nil {
			return nil, err
		}
	}
	for _, j := range d.jobs {
		d.states[str(j, "state")]++
	}
	for _, r := range d.rules {
		if r["enabled"] == false {
			d.disabled = append(d.disabled, str(r, "name"))
		}
	}
	sort.Strings(d.disabled)
	return d, nil
}

// json is the -o json shape of `status`.
func (d *statusData) json() map[string]any {
	return map[string]any{"sources": d.sources, "rules": map[string]any{"total": len(d.rules), "disabled": nonNil(d.disabled)},
		"approvals": len(d.approvals), "jobs": d.states}
}

func buildStatus(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		d, err := fetchStatus(c.call)
		if err != nil {
			return err
		}
		sources, rules, approvals, states, disabled := d.sources, d.rules, d.approvals, d.states, d.disabled
		if c.json() {
			return c.jsonOut(d.json())
		}
		fmt.Fprintf(c.out, "sources: %d\n", len(sources))
		for _, s := range sources {
			line := fmt.Sprintf("  %-20s %-6s %s", str(s, "name"), str(s, "health"), str(s, "last_error"))
			fmt.Fprintln(c.out, strings.TrimRight(line, " "))
		}
		fmt.Fprintf(c.out, "rules: %d", len(rules))
		if len(disabled) > 0 {
			fmt.Fprintf(c.out, " (disabled: %s)", strings.Join(disabled, ", "))
		}
		fmt.Fprintf(c.out, "\napprovals waiting: %d\njobs (last 100):", len(approvals))
		keys := make([]string, 0, len(states))
		for k := range states {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(c.out, " %d %s", states[k], k)
		}
		if len(keys) == 0 {
			fmt.Fprint(c.out, " none")
		}
		fmt.Fprintln(c.out)
		return nil
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func buildGet(fs *flag.FlagSet) func(*cli, []string) error {
	state := fs.String("state", "", "jobs: only this state")
	limit := fs.Int("limit", 0, "jobs, audit: at most this many rows")
	rule := fs.String("rule", "", "audit: only this rule's rows")
	event := fs.String("event", "", "audit: only this event")
	since := fs.String("since", "", "audit: since a time (RFC 3339) or how long ago (1h)")
	return func(c *cli, args []string) error {
		if len(args) < 1 || len(args) > 2 {
			return usageErr("usage: siphon get <kind> [name]", "kinds: "+strings.Join(append(slices.Clone(configKinds), listKinds...), ", "))
		}
		kind, name := args[0], ""
		if len(args) == 2 {
			name = args[1]
		}
		q := url.Values{}
		for k, v := range map[string]string{"state": *state, "rule": *rule, "event": *event, "since": *since} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		qs := ""
		if len(q) > 0 {
			qs = "?" + q.Encode()
		}
		switch {
		case slices.Contains(configKinds, kind):
			return c.getConfig(kind, name)
		case kind == "jobs" && name != "":
			return c.showJob(name)
		case kind == "jobs":
			return c.listJobs("/api/jobs" + qs)
		case kind == "approvals" || kind == "audit":
			var rows []map[string]any
			if err := c.call("GET", "/api/"+kind+qs, nil, &rows); err != nil {
				return err
			}
			if c.json() {
				return c.jsonOut(rows)
			}
			if kind == "approvals" {
				return c.table([]string{"JOB", "RULE", "EXPIRES"}, mapRows(rows, "job_id", "rule", "expires_at"))
			}
			return c.table([]string{"ID", "AT", "ACTOR", "EVENT", "JOB", "DETAIL"}, mapRows(rows, "id", "at", "actor", "event", "job_id", "detail"))
		case kind == "connections":
			var v map[string]any
			if err := c.call("GET", "/api/connections", nil, &v); err != nil {
				return err
			}
			if c.json() {
				return c.jsonOut(v)
			}
			logins, _ := v["logins"].([]any)
			models, _ := v["models"].([]any)
			fmt.Fprintln(c.out, "logins:")
			if err := c.table([]string{"NAME", "PROVIDER", "TYPE", "STATUS", "EXPIRY"}, anyRows(logins, "name", "provider", "type", "status", "expiry")); err != nil {
				return err
			}
			fmt.Fprintln(c.out, "\nmodels:")
			return c.table([]string{"NAME", "PROVIDER", "URL"}, anyRows(models, "name", "provider", "url"))
		}
		return usageErr("unknown kind "+strconv.Quote(kind), "kinds: "+strings.Join(append(slices.Clone(configKinds), listKinds...), ", "))
	}
}

// humanTime shows an RFC 3339 time in the viewer's zone, short; anything
// else comes back unchanged. JSON output keeps the API's UTC timestamps.
func humanTime(s string) string {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Local().Format("2006-01-02 15:04:05")
	}
	return s
}

func mapRows(rows []map[string]any, cols ...string) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		for _, k := range cols {
			v := humanTime(str(r, k))
			if v == "" {
				v = "-"
			}
			out[i] = append(out[i], v)
		}
	}
	return out
}

func anyRows(rows []any, cols ...string) [][]string {
	m := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if x, ok := r.(map[string]any); ok {
			m = append(m, x)
		}
	}
	return mapRows(m, cols...)
}

func (c *cli) getConfig(kind, name string) error {
	if name == "" {
		var rows []map[string]any
		if err := c.call("GET", "/api/config/"+kind, nil, &rows); err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(rows)
		}
		for _, r := range rows { // "portal" is how the API says: made through the portal or CLI, kept as revisions
			if r["provenance"] == "portal" {
				r["provenance"] = "live"
			}
		}
		if kind == "sources" { // the NEXT column: a schedule's next run
			var live []map[string]any
			if c.call("GET", "/api/sources", nil, &live) == nil {
				next := map[string]any{}
				for _, l := range live {
					next[str(l, "name")] = l["next_run_at"]
				}
				for _, r := range rows {
					if n, ok := next[str(r, "name")]; ok && n != nil {
						r["next"] = n
					}
				}
			}
			return c.table([]string{"NAME", "ORIGIN", "NEXT"}, mapRows(rows, "name", "provenance", "next"))
		}
		return c.table([]string{"NAME", "ORIGIN"}, mapRows(rows, "name", "provenance"))
	}
	var item map[string]any
	if err := c.call("GET", "/api/config/"+kind+"/"+url.PathEscape(name), nil, &item); err != nil {
		return err
	}
	if c.json() {
		return c.jsonOut(item)
	}
	fmt.Fprint(c.out, str(item, "yaml"))
	return nil
}

func (c *cli) listJobs(path string) error {
	var rows []map[string]any
	if err := c.call("GET", path, nil, &rows); err != nil {
		return err
	}
	if c.json() {
		return c.jsonOut(rows)
	}
	return c.table([]string{"ID", "RULE", "STATE", "ATTEMPT", "CREATED", "EXIT"}, mapRows(rows, "id", "rule", "state", "attempt", "created_at", "exit_code"))
}

func (c *cli) showJob(id string) error {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return usageErr("bad job id "+strconv.Quote(id), "ids come from `siphon jobs ls`")
	}
	var j map[string]any
	if err := c.call("GET", "/api/jobs/"+id, nil, &j); err != nil {
		return err
	}
	if c.json() {
		return c.jsonOut(j)
	}
	keys := make([]string, 0, len(j))
	for k := range j {
		if k != "output" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(c.out, "%-12s %s\n", k+":", humanTime(str(j, k)))
	}
	if o := str(j, "output"); o != "" {
		fmt.Fprintf(c.out, "output:\n%s\n", o)
	}
	return nil
}

func buildJobs(fs *flag.FlagSet) func(*cli, []string) error {
	state := fs.String("state", "", "only jobs in this state")
	limit := fs.Int("limit", 0, "at most this many jobs")
	return func(c *cli, args []string) error {
		if len(args) > 0 && args[0] == "show" {
			if err := needArgs(args, 2, "jobs show <id>"); err != nil {
				return err
			}
			return c.showJob(args[1])
		}
		if len(args) > 0 && args[0] != "ls" {
			return usageErr("usage: siphon jobs [ls|show <id>]", "")
		}
		q := url.Values{}
		if *state != "" {
			q.Set("state", *state)
		}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		p := "/api/jobs"
		if len(q) > 0 {
			p += "?" + q.Encode()
		}
		return c.listJobs(p)
	}
}

func buildDecide(verb string) func(*flag.FlagSet) func(*cli, []string) error {
	return func(fs *flag.FlagSet) func(*cli, []string) error {
		return func(c *cli, args []string) error {
			if err := needArgs(args, 1, verb+" <job id>"); err != nil {
				return err
			}
			if _, err := strconv.ParseInt(args[0], 10, 64); err != nil {
				return usageErr("bad job id "+strconv.Quote(args[0]), "ids come from `siphon get approvals`")
			}
			if err := c.call("POST", "/api/jobs/"+args[0]+"/"+verb, nil, nil); err != nil {
				return err
			}
			if c.json() {
				return c.jsonOut(map[string]any{"ok": true, "job": args[0], "decision": verb})
			}
			c.say("%sd job %s\n", verb, args[0])
			return nil
		}
	}
}

func buildToggle(verb string) func(*flag.FlagSet) func(*cli, []string) error {
	return func(fs *flag.FlagSet) func(*cli, []string) error {
		return func(c *cli, args []string) error {
			if err := needArgs(args, 1, verb+" <rule>"); err != nil {
				return err
			}
			if err := c.call("POST", "/api/rules/"+url.PathEscape(args[0])+"/"+verb, nil, nil); err != nil {
				return err
			}
			if c.json() {
				return c.jsonOut(map[string]any{"ok": true, "rule": args[0], "enabled": verb == "enable"})
			}
			c.say("%sd rule %s\n", verb, args[0])
			return nil
		}
	}
}

type applyItem = applyfile.Item

// parseApply reads an apply file; its errors are usage errors.
func parseApply(b []byte) ([]applyItem, error) { return applyfile.Parse(b) }

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

// checkSecretSpecs refuses bad --secret flags before any file or stdin is read.
func checkSecretSpecs(specs []string, stdinUsed bool) error {
	for _, s := range specs {
		k, v, ok := strings.Cut(s, "=")
		switch {
		case !ok || k == "" || !strings.Contains(k, "/") || !strings.Contains(k, "."):
			return usageErr("bad --secret "+strconv.Quote(k)+": want <kind>/<name>.<field>=@file or =-", "for example --secret sources/hook.secret=@hook.key")
		case v == "-" && stdinUsed:
			return usageErr("stdin is already used for the file", "give the secret as @file instead")
		case v != "-" && !strings.HasPrefix(v, "@"):
			return usageErr("refusing a secret value on the command line: it would end up in your shell history and the process list",
				"use --secret "+k+"=@file, or --secret "+k+"=- to read it from stdin")
		}
	}
	return nil
}

// readSecrets resolves --secret key=@file|key=- ; a plain value is refused.
func (c *cli) readSecrets(specs []string, stdinUsed bool) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range specs {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" || !strings.Contains(k, "/") || !strings.Contains(k, ".") {
			return nil, usageErr("bad --secret "+strconv.Quote(k)+": want <kind>/<name>.<field>=@file or =-", "for example --secret sources/hook.secret=@hook.key")
		}
		var raw []byte
		var err error
		switch {
		case v == "-":
			if stdinUsed {
				return nil, usageErr("stdin is already used for the file", "give the secret as @file instead")
			}
			stdinUsed = true
			raw, err = readAll(c)
		case strings.HasPrefix(v, "@"):
			raw, err = os.ReadFile(v[1:])
		default:
			return nil, usageErr("refusing a secret value on the command line: it would end up in your shell history and the process list",
				"use --secret "+k+"=@file, or --secret "+k+"=- to read it from stdin")
		}
		if err != nil {
			return nil, usageErr("cannot read the secret for "+k, "")
		}
		out[k] = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	}
	return out, nil
}

func readAll(c *cli) ([]byte, error) {
	if c.rd == nil {
		c.rd = bufio.NewReader(c.in)
	}
	return io.ReadAll(c.rd)
}

func buildApply(fs *flag.FlagSet) func(*cli, []string) error {
	file := fs.String("f", "", "file to apply, or - for stdin")
	dry := fs.Bool("dry-run", false, "check and show the diff, change nothing")
	yes := fs.Bool("yes", false, "apply without asking")
	var secrets multi
	fs.Var(&secrets, "secret", "a secret value as <kind>/<name>.<field>=@file or =- (stdin); never a plain value")
	return func(c *cli, args []string) error {
		if *file == "" || len(args) > 0 {
			return usageErr("usage: siphon apply -f <file|-> [--dry-run] [--secret k=@file|k=-] [--yes]", "write items in siphon.yaml shape: `siphon help apply`")
		}
		if err := checkSecretSpecs(secrets, *file == "-"); err != nil { // before anything is read
			return err
		}
		var b []byte
		var err error
		if *file == "-" {
			b, err = readAll(c)
		} else {
			b, err = os.ReadFile(*file)
		}
		if err != nil {
			return usageErr("cannot read "+*file, "")
		}
		items, err := parseApply(b)
		if err != nil {
			return err
		}
		sec, err := c.readSecrets(secrets, *file == "-")
		if err != nil {
			return err
		}
		out, err := c.runApply(items, sec, *dry, *yes, *file == "-")
		if err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(out)
		}
		return nil
	}
}

// applyOutcome is what an apply did (or, for a dry run, would do).
type applyOutcome struct {
	DryRun     bool     `json:"dry_run"`
	Changed    bool     `json:"changed"`
	Applied    bool     `json:"applied"`
	Rev        int64    `json:"rev,omitempty"`
	Diff       string   `json:"diff"`
	Errors     []string `json:"errors"`
	Warnings   []string `json:"warnings"`
	ApplyError string   `json:"apply_error,omitempty"`
}

// runApply is dry-run, diff, confirmation, apply: the one path under
// `apply` and `new task`. Text output is printed here; -o json is the caller's.
func (c *cli) runApply(items []applyItem, sec map[string]string, dry, yes, stdinUsed bool) (*applyOutcome, error) {
	body := map[string]any{"items": items}
	if len(sec) > 0 {
		body["secrets"] = sec
	}
	var check struct {
		Diff     string   `json:"diff"`
		Warnings []string `json:"warnings"`
	}
	if err := c.call("POST", "/api/config/apply?dry_run=1", body, &check); err != nil {
		return nil, err
	}
	out := &applyOutcome{DryRun: dry, Diff: check.Diff, Errors: []string{}, Warnings: nonNil(check.Warnings)}
	out.Changed = check.Diff != "" && check.Diff != "(no change)"
	if !c.json() {
		for _, w := range check.Warnings {
			fmt.Fprintln(c.errw, "warning:", w)
		}
	}
	if dry {
		if !c.json() {
			c.showDiff(check.Diff)
		}
		return out, nil
	}
	if !out.Changed {
		c.say("no change\n")
		return out, nil
	}
	if !yes {
		if c.json() { // -o json is never consent
			return nil, errNeedYes
		}
		if stdinUsed && !c.isTTY {
			return nil, usageErr("the file came from stdin, so there is nowhere to ask", "pass --yes, or --dry-run to only look")
		}
		c.showDiff(check.Diff)
		ok, err := c.confirm("Apply?")
		if err != nil {
			return nil, err
		}
		if !ok {
			c.say("not applied\n")
			out.Changed = false
			return out, nil
		}
	}
	var res struct {
		Rev        int64  `json:"rev"`
		Applied    bool   `json:"applied"`
		ApplyError string `json:"apply_error"`
	}
	if err := c.call("POST", "/api/config/apply", body, &res); err != nil {
		return nil, err
	}
	out.Rev, out.Applied, out.ApplyError = res.Rev, res.Applied, res.ApplyError
	if !res.Applied {
		return nil, &client.Error{Msg: fmt.Sprintf("saved as revision %d but could not be applied live: %s", res.Rev, res.ApplyError), Hint: "restart siphon to apply it, or `siphon restore` an earlier revision"}
	}
	c.say("applied revision %d (%d items)\n", res.Rev, len(items))
	return out, nil
}

func (c *cli) showDiff(d string) {
	if d == "" {
		d = "(no change)"
	}
	fmt.Fprintln(c.out, strings.TrimRight(d, "\n"))
}

func buildEdit(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		if err := needArgs(args, 2, "edit <kind> <name>"); err != nil {
			return err
		}
		kind, name := args[0], args[1]
		if err := checkKind(kind); err != nil {
			return err
		}
		path := "/api/config/" + kind + "/" + url.PathEscape(name)
		fetch := func() (yamlText string, rev *int64, err error) {
			var cur struct {
				YAML string `json:"yaml"`
				Rev  int64  `json:"rev"`
			}
			if err = c.call("GET", path, nil, &cur); err != nil {
				var ce *client.Error
				if asClientErr(err, &ce) && ce.Status == 404 { // a new item
					return "", nil, nil
				}
				return "", nil, err
			}
			return cur.YAML, &cur.Rev, nil
		}
		text, rev, err := fetch()
		if err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "siphon-edit-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		file := filepath.Join(dir, kind+"-"+name+".yaml")
		for {
			if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
				return err
			}
			if err := c.edit(file); err != nil {
				return &client.Error{Msg: "the editor failed: " + err.Error(), Hint: "set $EDITOR to an editor that waits, like `code -w`"}
			}
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			edited := string(b)
			if edited == text {
				c.say("no change\n")
				return nil
			}
			var res struct {
				Rev int64 `json:"rev"`
			}
			err = c.call("PUT", path, map[string]any{"yaml": edited, "rev": rev}, &res)
			if err == nil {
				if c.json() {
					return c.jsonOut(res)
				}
				c.say("saved %s/%s as revision %d\n", kind, name, res.Rev)
				return nil
			}
			var ce *client.Error
			if !asClientErr(err, &ce) || (ce.Status != 409 && ce.Status != 422) {
				return err
			}
			saved, serr := keepCopy(edited)
			if serr == nil {
				ce.Hint = "your edit is kept in " + saved + ". " + ce.Hint
			}
			if !c.isTTY {
				return err
			}
			if ce.Status == 409 {
				fmt.Fprintf(c.errw, "%s/%s changed on the server while you edited it. Your text is kept in %s; reopening the current version.\n", kind, name, saved)
				if text, rev, err = fetch(); err != nil {
					return err
				}
				continue
			}
			c.report(err)
			ok, _ := c.confirm("Edit again?")
			if !ok {
				return err
			}
			text = edited // keep the user's text; rev is still current
		}
	}
}

func asClientErr(err error, target **client.Error) bool {
	ce, ok := err.(*client.Error)
	if ok {
		*target = ce
	}
	return ok
}

func keepCopy(s string) (string, error) {
	f, err := os.CreateTemp("", "siphon-edit-kept-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return f.Name(), err
}

func (c *cli) edit(path string) error {
	if c.runEditor != nil {
		return c.runEditor(path)
	}
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	cmd := exec.Command("sh", "-c", ed+` "$1"`, "siphon-edit", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}

func buildDelete(fs *flag.FlagSet) func(*cli, []string) error { return buildChange(fs, "delete") }
func buildReset(fs *flag.FlagSet) func(*cli, []string) error  { return buildChange(fs, "reset") }

func buildChange(fs *flag.FlagSet, verb string) func(*cli, []string) error {
	dry := fs.Bool("dry-run", false, "check and show the diff, change nothing")
	return func(c *cli, args []string) error {
		if err := needArgs(args, 2, verb+" <kind> <name> [--dry-run]"); err != nil {
			return err
		}
		if err := checkKind(args[0]); err != nil {
			return err
		}
		method, path := "DELETE", "/api/config/"+args[0]+"/"+url.PathEscape(args[1])
		if verb == "reset" {
			method, path = "POST", path+"/reset"
		}
		if *dry {
			var check struct {
				Diff string `json:"diff"`
			}
			if err := c.call(method, path+"?dry_run=1", nil, &check); err != nil {
				return err
			}
			if c.json() {
				return c.jsonOut(map[string]any{"dry_run": true, "diff": check.Diff, "errors": []string{}, "warnings": []string{}})
			}
			c.showDiff(check.Diff)
			return nil
		}
		var res struct {
			Rev        int64  `json:"rev"`
			Applied    bool   `json:"applied"`
			ApplyError string `json:"apply_error"`
		}
		if err := c.call(method, path, nil, &res); err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(res)
		}
		if !res.Applied {
			return &client.Error{Msg: fmt.Sprintf("saved as revision %d but could not be applied live: %s", res.Rev, res.ApplyError), Hint: "restart siphon to apply it"}
		}
		c.say("%s %s/%s (revision %d)\n", map[string]string{"delete": "deleted", "reset": "reset"}[verb], args[0], args[1], res.Rev)
		return nil
	}
}

func buildHistory(fs *flag.FlagSet) func(*cli, []string) error {
	return func(c *cli, args []string) error {
		if len(args) == 1 {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return usageErr("usage: siphon history [kind name | <rev>]", "")
			}
			var rev map[string]any
			if err := c.call("GET", "/api/config/history/"+strconv.FormatInt(id, 10), nil, &rev); err != nil {
				return err
			}
			if c.json() {
				return c.jsonOut(rev)
			}
			fmt.Fprintf(c.out, "revision %s by %s at %s: %s\n\n%s", str(rev, "id"), str(rev, "actor"), humanTime(str(rev, "at")), str(rev, "summary"), str(rev, "diff"))
			return nil
		}
		if len(args) != 0 && len(args) != 2 {
			return usageErr("usage: siphon history [kind name | <rev>]", "")
		}
		var rows []map[string]any
		if err := c.call("GET", "/api/config/history", nil, &rows); err != nil {
			return err
		}
		if len(args) == 2 {
			if err := checkKind(args[0]); err != nil {
				return err
			}
			prefix := args[0] + "/" + args[1] + " "
			rows = slices.DeleteFunc(rows, func(r map[string]any) bool { return !strings.Contains(str(r, "summary"), prefix) })
		}
		if c.json() {
			return c.jsonOut(nonNilRows(rows))
		}
		return c.table([]string{"REV", "AT", "ACTOR", "SUMMARY"}, mapRows(rows, "id", "at", "actor", "summary"))
	}
}

func nonNilRows(r []map[string]any) []map[string]any {
	if r == nil {
		return []map[string]any{}
	}
	return r
}

func buildRestore(fs *flag.FlagSet) func(*cli, []string) error {
	yes := fs.Bool("yes", false, "restore without asking")
	return func(c *cli, args []string) error {
		if err := needArgs(args, 1, "restore <rev> [--yes]"); err != nil {
			return err
		}
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return usageErr("bad revision "+strconv.Quote(args[0]), "revisions come from `siphon history`")
		}
		p := "/api/config/history/" + strconv.FormatInt(id, 10)
		var rev map[string]any
		if err := c.call("GET", p, nil, &rev); err != nil {
			return err
		}
		if !*yes {
			if c.json() {
				return errNeedYes
			}
			fmt.Fprintf(c.out, "revision %d: %s (by %s)\n", id, str(rev, "summary"), str(rev, "actor"))
			ok, err := c.confirm("Put the config back to this revision?")
			if err != nil {
				return err
			}
			if !ok {
				c.say("not restored\n")
				return nil
			}
		}
		if err := c.call("POST", p+"/restore", nil, nil); err != nil {
			return err
		}
		if c.json() {
			return c.jsonOut(map[string]any{"ok": true, "restored": id})
		}
		c.say("restored revision %d as a new revision\n", id)
		return nil
	}
}
