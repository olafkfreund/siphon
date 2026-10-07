package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/client"
	"github.com/olafkfreund/siphon/internal/draft"
)

// draftCall asks the server to draft an apply file. A model can take minutes.
func (c *cli) draftCall(request, connection, model string) (map[string]any, error) {
	cl, err := c.api()
	if err != nil {
		return nil, err
	}
	cl.HTTP.Timeout = 8 * time.Minute
	var res map[string]any
	err = cl.Do("POST", "/api/draft", map[string]any{"request": request, "connection": connection, "model": model}, &res)
	return res, err
}

func strList(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func buildDraft(fs *flag.FlagSet) func(*cli, []string) error {
	connection := fs.String("connection", "", "the model connection to ask (default: the first one)")
	model := fs.String("model", "", "the model id (default: the connection's first listed model)")
	apply := fs.Bool("apply", false, "apply the draft after showing the diff and asking")
	yes := fs.Bool("yes", false, "with --apply: don't ask")
	return func(c *cli, args []string) error {
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return usageErr(`usage: siphon draft "<what you want>" [--connection c] [--model m] [--apply] [--yes]`, `describe the task in words, for example: siphon draft "tell me on ntfy when a deploy webhook says failed"`)
		}
		res, err := c.draftCall(args[0], *connection, *model)
		if err != nil {
			return err
		}
		errs := strList(res["errors"])
		yamlText := str(res, "yaml")
		items, perr := parseApply([]byte(yamlText))
		var sec map[string]string
		var hooks []map[string]string
		generated := map[string]bool{}
		if *apply && len(errs) == 0 && perr == nil {
			sec = map[string]string{}
			for _, s := range draft.WebhookNeedsSecret(items) {
				b := make([]byte, 32)
				rand.Read(b)
				sec[s+".secret"] = hex.EncodeToString(b)
				generated[s+".secret"] = true
			}
		}
		var todo []string
		for _, t := range strList(res["todo"]) {
			skip := false
			for g := range generated {
				skip = skip || strings.HasPrefix(t, "secret "+g+":")
			}
			if !skip {
				todo = append(todo, t)
			}
		}
		res["todo"] = nonNil(todo)
		if !c.json() {
			fmt.Fprint(c.out, yamlText)
			if len(errs) == 0 && !*apply {
				fmt.Fprint(c.out, "\nChanges:\n")
				for _, p := range strList(res["placeholders"]) {
					name := strings.TrimPrefix(p, "sources/")
					hint, _, _ := strings.Cut(strings.ReplaceAll(draft.ConnectHint(name), "`", ""), " (")
					fmt.Fprintf(c.out, "(%s: placeholder, created by %s)\n", name, hint)
				}
				fmt.Fprintln(c.out, strings.TrimRight(str(res, "diff"), "\n"))
			}
		}
		placeholders := strList(res["placeholders"])
		blocked := *apply && len(errs) == 0 && len(placeholders) > 0
		var outcome *applyOutcome
		if len(errs) == 0 && *apply && !blocked {
			if perr != nil {
				return perr
			}
			if outcome, err = c.runApply(items, sec, false, *yes, false); err != nil {
				return err
			}
			if outcome.Applied {
				cl, _ := c.api()
				for k, v := range sec {
					name := strings.TrimSuffix(strings.TrimPrefix(k, "sources/"), ".secret")
					hooks = append(hooks, map[string]string{"source": name, "url": cl.URL + "/hook/" + name, "secret": v})
					c.say("webhook source %q\n  URL:    %s\n  secret: %s\n          (shown once, copy it now)\n", name, cl.URL+"/hook/"+name, v)
				}
			}
		}
		if c.json() {
			if outcome != nil {
				res["apply"], res["webhooks"] = outcome, hooks
			}
			if err := c.jsonOut(res); err != nil {
				return err
			}
		} else if len(todo) > 0 {
			fmt.Fprintln(c.out, "\nTo do:")
			for _, t := range todo {
				fmt.Fprintln(c.out, "  -", t)
			}
		}
		if blocked {
			return &client.Error{Msg: "not applied: the draft uses sources that do not exist yet: " + strings.Join(placeholders, ", "), Code: client.ExitInvalid,
				Hint: "connect them first (see the to-do list), then run the draft again with --apply"}
		}
		if len(errs) > 0 {
			return &client.Error{Msg: fmt.Sprintf("the draft still has problems after %s rounds", str(res, "rounds")), Errors: errs, Code: client.ExitInvalid,
				Hint: "fix the YAML by hand and check it with `siphon apply -f file --dry-run`, or ask again with more detail"}
		}
		return nil
	}
}
