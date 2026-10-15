package main

import (
	"context"
	"encoding/json"
	"flag"
	"maps"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/docs"
	"github.com/olafkfreund/siphon/internal/client"
	"github.com/olafkfreund/siphon/internal/config"
)

// `siphon mcp` is an MCP server on stdio. Its tools are the read verbs of the
// CLI plus apply and delete, which only dry-run unless --allow-write is given.
// There are no approve or deny tools: a person decides those.

const writesOff = "writes are disabled; the user can restart siphon mcp with --allow-write"

func buildMCP(fs *flag.FlagSet) func(*cli, []string) error {
	allowWrite := fs.Bool("allow-write", false, "let apply and delete really change things (default: they only dry-run)")
	allowSecrets := fs.Bool("allow-secrets", false, "with --allow-write: accept secret values through apply (they pass through the assistant's context)")
	allowUnapproved := fs.Bool("allow-unapproved", false, "let apply turn an agent's approval off (default: refused)")
	return func(c *cli, args []string) error {
		if len(args) > 0 {
			return usageErr("usage: siphon mcp [--allow-write] [--allow-secrets] [--allow-unapproved]", "")
		}
		if *allowSecrets && !*allowWrite {
			return usageErr("--allow-secrets needs --allow-write", "")
		}
		c.actor = client.ActorLabel(":mcp")
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := newMCPServer(c, *allowWrite, *allowSecrets, *allowUnapproved).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	}
}

// result is a tool result: the same JSON as `-o json`, as text content.
func result(v any, isErr bool) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, IsError: isErr}
}

// fail turns an error into an isError result with {error, errors, hint}.
func fail(err error) *mcp.CallToolResult {
	ce, ok := err.(*client.Error)
	if !ok {
		ce = &client.Error{Msg: err.Error()}
	}
	if ce.Errors == nil {
		ce.Errors = []string{}
	}
	return result(ce, true)
}

type none struct{}

type getIn struct {
	Kind  string `json:"kind" jsonschema:"sources, rules, agents, routines, credentials, notify, jobs, approvals, audit, connections, egress or notifications"`
	Name  string `json:"name,omitempty" jsonschema:"one item's name (for jobs: its id); omit to list"`
	Rule  string `json:"rule,omitempty" jsonschema:"audit: only this rule's rows"`
	Event string `json:"event,omitempty" jsonschema:"audit: only this event"`
	Since string `json:"since,omitempty" jsonschema:"audit: since a time (RFC 3339) or how long ago (1h)"`
	Limit int    `json:"limit,omitempty" jsonschema:"jobs, audit, notifications: at most this many rows"`
}
type ruleIn struct {
	Rule string `json:"rule"`
}
type resetIn struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type historyIn struct {
	Kind string `json:"kind,omitempty" jsonschema:"with name: only this item's revisions"`
	Name string `json:"name,omitempty"`
	Rev  int64  `json:"rev,omitempty" jsonschema:"one revision, with its diff"`
}
type restoreIn struct {
	Rev int64 `json:"rev"`
}
type catalogIn struct {
	Category string `json:"category,omitempty" jsonschema:"only this category (code, issues, chat, monitoring, cloud, payments, home, generic)"`
}
type explainIn struct {
	Kind string `json:"kind" jsonschema:"source, rule, agent, routine, credential or notify"`
}
type templateIn struct {
	Name string `json:"name,omitempty" jsonschema:"a template name; omit to list them"`
}
type testIn struct {
	Rule    string            `json:"rule"`
	Event   map[string]any    `json:"event,omitempty" jsonschema:"a JSON event to run the rule against"`
	Headers map[string]string `json:"headers,omitempty"`
	UseLast bool              `json:"use_last,omitempty" jsonschema:"use the last event the rule's source produced"`
	At      string            `json:"at,omitempty" jsonschema:"schedule rules: build the event for this moment, like 2026-03-02 06:00 in the source's zone"`
}
type whyIn struct {
	Rule string `json:"rule"`
}
type jobsIn struct {
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type notifyTestIn struct {
	Name string `json:"name" jsonschema:"the notification channel to send a test message to"`
}
type jobIn struct {
	ID     int64 `json:"id"`
	Detail bool  `json:"detail,omitempty" jsonschema:"also the steps, prompt, hosts, failure summary and notifications"`
}
type applyIn struct {
	YAML    string            `json:"yaml" jsonschema:"items in siphon.yaml shape: sections sources, agents, routines, credentials, notify, rules"`
	Secrets map[string]string `json:"secrets,omitempty" jsonschema:"<kind>/<name>.<field> to value; refused unless siphon mcp runs with --allow-write and --allow-secrets"`
	DryRun  bool              `json:"dry_run,omitempty" jsonschema:"only check and show the diff"`
}
type draftIn struct {
	Request    string `json:"request" jsonschema:"what the user wants, in plain words"`
	Connection string `json:"connection,omitempty" jsonschema:"the model connection to ask; default the first"`
	Model      string `json:"model,omitempty"`
}
type deleteIn struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	DryRun bool   `json:"dry_run,omitempty"`
}

func newMCPServer(c *cli, allowWrite, allowSecrets, allowUnapproved bool) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "siphon", Version: version}, nil)
	get := func(path string) (*mcp.CallToolResult, any, error) {
		var v any
		if err := c.call("GET", path, nil, &v); err != nil {
			return fail(err), nil, nil
		}
		return result(v, false), nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "inventory", Description: "Names of everything configured (sources, rules, agents, routines, connections, MCP packages) and the operator's allowlists. Never secrets. Call this before writing a task."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			return get("/api/inventory")
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get", Description: "List config items of a kind, or show one item's YAML. Kinds: sources, rules, agents, routines, credentials, notify; also jobs, approvals, audit (filters rule, event, since, limit), connections, egress, notifications."},
		func(_ context.Context, _ *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, any, error) {
			n := url.PathEscape(in.Name)
			switch {
			case slices.Contains(configKinds, in.Kind) && in.Name == "":
				return get("/api/config/" + in.Kind)
			case slices.Contains(configKinds, in.Kind):
				return get("/api/config/" + in.Kind + "/" + n)
			case in.Kind == "jobs" && in.Name != "":
				if _, err := strconv.ParseInt(in.Name, 10, 64); err != nil {
					return fail(usageErr("a job name is its numeric id", "")), nil, nil
				}
				return get("/api/jobs/" + in.Name)
			case slices.Contains(listKinds, in.Kind) || in.Kind == "notifications":
				q := url.Values{}
				for k, v := range map[string]string{"rule": in.Rule, "event": in.Event, "since": in.Since} {
					if v != "" {
						q.Set(k, v)
					}
				}
				if in.Limit > 0 {
					q.Set("limit", strconv.Itoa(in.Limit))
				}
				p := "/api/" + in.Kind
				if len(q) > 0 {
					p += "?" + q.Encode()
				}
				return get(p)
			}
			return fail(usageErr("unknown kind "+strconv.Quote(in.Kind), "kinds: "+strings.Join(append(slices.Clone(configKinds), listKinds...), ", "))), nil, nil
		})
	toggle := func(verb string) func(context.Context, *mcp.CallToolRequest, ruleIn) (*mcp.CallToolResult, any, error) {
		return func(_ context.Context, _ *mcp.CallToolRequest, in ruleIn) (*mcp.CallToolResult, any, error) {
			if !allowWrite {
				return result(map[string]any{"would": verb + " rule " + in.Rule, "changed": false, "note": writesOff}, false), nil, nil
			}
			if err := c.call("POST", "/api/rules/"+url.PathEscape(in.Rule)+"/"+verb, nil, nil); err != nil {
				return fail(err), nil, nil
			}
			return result(map[string]any{"ok": true, "rule": in.Rule, "enabled": verb == "enable"}, false), nil, nil
		}
	}
	mcp.AddTool(s, &mcp.Tool{Name: "enable", Description: "Turn a rule on (runtime override). Without --allow-write on the server this does nothing; the result says so."}, toggle("enable"))
	mcp.AddTool(s, &mcp.Tool{Name: "disable", Description: "Turn a rule off (runtime override). Without --allow-write on the server this does nothing; the result says so."}, toggle("disable"))
	mcp.AddTool(s, &mcp.Tool{Name: "reset", Description: "Drop the portal's change to a config item, back to siphon.yaml. Without --allow-write on the server this only shows the diff; the result says so."},
		func(_ context.Context, _ *mcp.CallToolRequest, in resetIn) (*mcp.CallToolResult, any, error) {
			if !slices.Contains(configKinds, in.Kind) {
				return fail(usageErr("unknown kind "+strconv.Quote(in.Kind), "kinds: "+strings.Join(configKinds, ", "))), nil, nil
			}
			path := "/api/config/" + in.Kind + "/" + url.PathEscape(in.Name) + "/reset"
			var v map[string]any
			if !allowWrite {
				if err := c.call("POST", path+"?dry_run=1", nil, &v); err != nil {
					return fail(err), nil, nil
				}
				v["dry_run"], v["note"] = true, writesOff
				return result(v, false), nil, nil
			}
			if err := c.call("POST", path, nil, &v); err != nil {
				return fail(err), nil, nil
			}
			v["dry_run"] = false
			return result(v, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "history", Description: "Config revisions, newest first. With rev: that revision and its diff. With kind and name: only that item's revisions."},
		func(_ context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, any, error) {
			if in.Rev > 0 {
				return get("/api/config/history/" + strconv.FormatInt(in.Rev, 10))
			}
			var rows []map[string]any
			if err := c.call("GET", "/api/config/history", nil, &rows); err != nil {
				return fail(err), nil, nil
			}
			if in.Kind != "" || in.Name != "" {
				if !slices.Contains(configKinds, in.Kind) || in.Name == "" {
					return fail(usageErr("give both kind and name", "kinds: "+strings.Join(configKinds, ", "))), nil, nil
				}
				prefix := in.Kind + "/" + in.Name + " "
				rows = slices.DeleteFunc(rows, func(r map[string]any) bool { return !strings.Contains(str(r, "summary"), prefix) })
			}
			return result(nonNilRows(rows), false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "restore", Description: "Put the config overlay back as it was at a revision (a new revision). Refused without --allow-write on the server."},
		func(_ context.Context, _ *mcp.CallToolRequest, in restoreIn) (*mcp.CallToolResult, any, error) {
			if !allowWrite {
				return fail(usageErr(writesOff, "ask the user to run `siphon restore "+strconv.FormatInt(in.Rev, 10)+"`")), nil, nil
			}
			if err := c.call("POST", "/api/config/history/"+strconv.FormatInt(in.Rev, 10)+"/restore", nil, nil); err != nil {
				return fail(err), nil, nil
			}
			return result(map[string]any{"ok": true, "restored": in.Rev}, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "catalog", Description: "The services you can connect, by category, with what each needs on this install."},
		func(_ context.Context, _ *mcp.CallToolRequest, in catalogIn) (*mcp.CallToolResult, any, error) {
			all, err := c.catalog()
			if err != nil {
				return fail(err), nil, nil
			}
			var shown []catalogEntry
			for _, e := range all {
				if in.Category == "" || e.Category == in.Category {
					shown = append(shown, e)
				}
			}
			return result(map[string]any{"services": nonNilEntries(shown)}, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "explain", Description: "Every field of a config kind with its type, whether it is required, its default and allowed values."},
		func(_ context.Context, _ *mcp.CallToolRequest, in explainIn) (*mcp.CallToolResult, any, error) {
			kind, fields, err := config.Explain(in.Kind)
			if err != nil {
				return fail(usageErr(err.Error(), "")), nil, nil
			}
			return result(map[string]any{"kind": kind, "fields": fields}, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "template", Description: "Ready-made task templates. Without a name: the list. With a name: that template's YAML and header fields (needs, secrets, apply command, notes)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in templateIn) (*mcp.CallToolResult, any, error) {
			if in.Name == "" {
				ts := docs.Templates()
				for i := range ts {
					ts[i].YAML = ""
				}
				return result(ts, false), nil, nil
			}
			t, ok := docs.Lookup(in.Name)
			if !ok {
				return fail(&client.Error{Status: 404, Msg: "no template named " + in.Name, Hint: closeTemplates(in.Name)}), nil, nil
			}
			return result(t, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "test", Description: "Dry-run a rule against an event (or the source's last event with use_last) and see what it would run. Changes nothing."},
		func(_ context.Context, _ *mcp.CallToolRequest, in testIn) (*mcp.CallToolResult, any, error) {
			body := map[string]any{"use_last": in.UseLast}
			if in.Event != nil {
				body["event"] = in.Event
			}
			if in.At != "" {
				body["at"] = in.At
			}
			if len(in.Headers) > 0 {
				body["headers"] = in.Headers
			}
			var v any
			if err := c.call("POST", "/api/rules/"+url.PathEscape(in.Rule)+"/test", body, &v); err != nil {
				return fail(err), nil, nil
			}
			return result(v, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "why", Description: "Why a rule did or did not fire: its state, the source's health, the last event, and the likely reasons, most likely first."},
		func(_ context.Context, _ *mcp.CallToolRequest, in whyIn) (*mcp.CallToolResult, any, error) {
			return get("/api/rules/" + url.PathEscape(in.Rule) + "/explain")
		})
	mcp.AddTool(s, &mcp.Tool{Name: "jobs", Description: "Recent jobs, newest first, optionally only one state (queued, running, done, failed, pending_approval)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in jobsIn) (*mcp.CallToolResult, any, error) {
			q := url.Values{}
			if in.State != "" {
				q.Set("state", in.State)
			}
			if in.Limit > 0 {
				q.Set("limit", strconv.Itoa(in.Limit))
			}
			p := "/api/jobs"
			if len(q) > 0 {
				p += "?" + q.Encode()
			}
			return get(p)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "job", Description: "One job: state, exit code and output; detail also gives the steps, prompt, hosts, failure summary and notifications."},
		func(_ context.Context, _ *mcp.CallToolRequest, in jobIn) (*mcp.CallToolResult, any, error) {
			p := "/api/jobs/" + strconv.FormatInt(in.ID, 10)
			if in.Detail {
				p += "?detail=1"
			}
			return get(p)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "status", Description: "Sources and their health, rule counts, pending approvals and recent job states."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			d, err := fetchStatus(c.call)
			if err != nil {
				return fail(err), nil, nil
			}
			return result(d.json(), false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "guide", Description: "The guide for assistants: how to write tasks, the rules, and the CLI workflow. Read it first."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			b, ok := docs.Page("llm")
			if !ok {
				return fail(&client.Error{Msg: "this build has no guide"}), nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "apply", Description: "Create or update items from YAML (all or nothing). Without --allow-write on the server this only dry-runs and shows the diff; the result says so."},
		func(_ context.Context, _ *mcp.CallToolRequest, in applyIn) (*mcp.CallToolResult, any, error) {
			if len(in.Secrets) > 0 && !(allowWrite && allowSecrets) {
				return fail(usageErr("secret values are not accepted through MCP: they would pass through the assistant's context",
					"ask the user to run `siphon apply -f file.yaml --secret <kind>/<name>.<field>=-` in a terminal, or restart siphon mcp with --allow-write --allow-secrets")), nil, nil
			}
			items, err := parseApply([]byte(in.YAML))
			if err != nil {
				return fail(err), nil, nil
			}
			if err := mcpGuard(c, items, allowSecrets, allowUnapproved); err != nil {
				return fail(err), nil, nil
			}
			body := map[string]any{"items": items}
			if len(in.Secrets) > 0 {
				body["secrets"] = in.Secrets
			}
			var check struct {
				Diff     string   `json:"diff"`
				Warnings []string `json:"warnings"`
			}
			if err := c.call("POST", "/api/config/apply?dry_run=1", body, &check); err != nil {
				return fail(err), nil, nil
			}
			out := map[string]any{"diff": check.Diff, "errors": []string{}, "warnings": nonNil(check.Warnings)}
			if in.DryRun || !allowWrite {
				out["dry_run"] = true
				if !in.DryRun {
					out["note"] = writesOff
				}
				return result(out, false), nil, nil
			}
			var res map[string]any
			if err := c.call("POST", "/api/config/apply", body, &res); err != nil {
				return fail(err), nil, nil
			}
			res["dry_run"], res["diff"] = false, check.Diff
			return result(res, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "notify_test", Description: "Send one test message to a notification channel and report the HTTP status or the masked error. Without --allow-write on the server this does nothing; the result says so."},
		func(_ context.Context, _ *mcp.CallToolRequest, in notifyTestIn) (*mcp.CallToolResult, any, error) {
			if !allowWrite {
				return result(map[string]any{"sent": false, "note": writesOff}, false), nil, nil
			}
			var v map[string]any
			if err := c.call("POST", "/api/notify/"+url.PathEscape(in.Name)+"/test", nil, &v); err != nil {
				return fail(err), nil, nil
			}
			return result(v, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "draft", Description: "Ask a model connection to draft an apply file from plain words. It is checked (diff, errors, a to-do list) but NEVER applied, even with --allow-write: review it, then call apply."},
		func(_ context.Context, _ *mcp.CallToolRequest, in draftIn) (*mcp.CallToolResult, any, error) {
			res, err := c.draftCall(in.Request, in.Connection, in.Model)
			if err != nil {
				return fail(err), nil, nil
			}
			return result(res, false), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "delete", Description: "Delete a config item (a file item is hidden, a portal item removed). Without --allow-write on the server this only dry-runs; the result says so."},
		func(_ context.Context, _ *mcp.CallToolRequest, in deleteIn) (*mcp.CallToolResult, any, error) {
			if !slices.Contains(configKinds, in.Kind) {
				return fail(usageErr("unknown kind "+strconv.Quote(in.Kind), "kinds: "+strings.Join(configKinds, ", "))), nil, nil
			}
			path := "/api/config/" + in.Kind + "/" + url.PathEscape(in.Name)
			var v map[string]any
			if in.DryRun || !allowWrite {
				if err := c.call("DELETE", path+"?dry_run=1", nil, &v); err != nil {
					return fail(err), nil, nil
				}
				v["dry_run"] = true
				if !in.DryRun {
					v["note"] = writesOff
				}
				return result(v, false), nil, nil
			}
			if err := c.call("DELETE", path, nil, &v); err != nil {
				return fail(err), nil, nil
			}
			v["dry_run"] = false
			return result(v, false), nil, nil
		})
	return s
}

// mcpGuard refuses what an assistant should not do unasked: turn an agent's
// approval off, or put a plain value (maybe a token) in a source's headers.
func mcpGuard(c *cli, items []applyItem, allowSecrets, allowUnapproved bool) error {
	for _, it := range items {
		switch it.Kind {
		case "sources":
			var s struct {
				Headers map[string]string `yaml:"headers"`
			}
			if yaml.Unmarshal([]byte(it.YAML), &s) != nil || allowSecrets {
				continue
			}
			for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
				if v := s.Headers[k]; !strings.HasPrefix(v, "env:") && !strings.HasPrefix(v, "file:") {
					return usageErr("sources/"+it.Name+": header "+k+" holds a plain value, which may be a token",
						"use an env: or file: ref and have the user supply the secret, or restart siphon mcp with --allow-write --allow-secrets")
				}
			}
		case "agents":
			var n struct {
				Approve *bool `yaml:"approve"`
			}
			if allowUnapproved || yaml.Unmarshal([]byte(it.YAML), &n) != nil || n.Approve == nil || *n.Approve {
				continue
			}
			var cur struct {
				YAML string `json:"yaml"`
			}
			cerr := c.call("GET", "/api/config/agents/"+url.PathEscape(it.Name), nil, &cur)
			var old struct {
				Approve *bool `yaml:"approve"`
			}
			if cerr == nil {
				yaml.Unmarshal([]byte(cur.YAML), &old)
			} else if ce, ok := cerr.(*client.Error); !ok || ce.Status != 404 {
				return cerr
			}
			if old.Approve == nil || *old.Approve { // true, or the default (true)
				return usageErr("agents/"+it.Name+": approve: false would let it run without a person's approval",
					"ask the user to make this change themselves, or restart siphon mcp with --allow-unapproved")
			}
		}
	}
	return nil
}
