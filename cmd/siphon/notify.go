package main

import (
	"flag"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/client"
	"github.com/olafkfreund/siphon/internal/config"
)

// buildNotify is `siphon notify add|test|log`. Channels are plain config
// items (`get`, `delete` and `history` work on kind notify); add stores the
// url and token as write-only secrets.
func buildNotify(fs *flag.FlagSet) func(*cli, []string) error {
	typ := fs.String("type", "", "add: ntfy, slack or webhook")
	urlFlag := fs.String("url", "", "add: the topic or hook URL, as - (stdin) or @file; never a plain value")
	token := fs.String("token", "", "add: an ntfy access token or webhook bearer, as - (stdin) or @file (optional)")
	events := fs.String("events", "", "add: comma list of approval,reminder,failed,source (default all four)")
	dry := fs.Bool("dry-run", false, "add: check and show the diff, change nothing")
	yes := fs.Bool("yes", false, "add: apply without asking")
	channel := fs.String("channel", "", "log: only this channel")
	job := fs.Int64("job", 0, "log: only this job's notifications")
	limit := fs.Int("limit", 0, "log: at most this many rows")
	const usage = "usage: siphon notify add <name> --type ntfy|slack|webhook --url -|@file [--token -|@file] [--events a,b] | test <name> | log"
	return func(c *cli, args []string) error {
		if len(args) == 0 {
			return usageErr(usage, "run `siphon help notify` for the flags")
		}
		switch args[0] {
		case "add":
			if len(args) != 2 || *typ == "" || *urlFlag == "" {
				return usageErr(usage, "")
			}
			return c.notifyAdd(args[1], *typ, *urlFlag, *token, *events, *dry, *yes)
		case "test":
			if len(args) != 2 {
				return usageErr(usage, "")
			}
			return c.notifyTest(args[1])
		case "log":
			if len(args) != 1 {
				return usageErr(usage, "")
			}
			return c.notifyLog(*channel, *job, *limit)
		}
		return usageErr("unknown notify command "+strconv.Quote(args[0]), usage)
	}
}

func (c *cli) notifyAdd(name, typ, urlArg, tokenArg, events string, dry, yes bool) error {
	if !slices.Contains(config.NotifyTypes, typ) {
		return usageErr("bad --type "+strconv.Quote(typ), "types: "+strings.Join(config.NotifyTypes, ", "))
	}
	item := map[string]any{"type": typ}
	if events != "" {
		item["events"] = strings.Split(events, ",")
	}
	var specs []string
	for _, f := range []struct{ flag, field, v string }{{"--url", "url", urlArg}, {"--token", "token", tokenArg}} {
		switch {
		case f.v == "":
		case f.v == "-" || strings.HasPrefix(f.v, "@"):
			specs = append(specs, "notify/"+name+"."+f.field+"="+f.v)
		default:
			return usageErr("refusing a secret value on the command line: it would end up in your shell history and the process list",
				"use "+f.flag+" @file, or "+f.flag+" - to read it from stdin")
		}
	}
	stdin := urlArg == "-" || tokenArg == "-"
	if urlArg == "-" && tokenArg == "-" {
		return usageErr("stdin can hold only one secret", "give one of them as @file")
	}
	if err := checkSecretSpecs(specs, false); err != nil {
		return err
	}
	sec, err := c.readSecrets(specs, false)
	if err != nil {
		return err
	}
	y, err := yaml.Marshal(item)
	if err != nil {
		return err
	}
	out, err := c.runApply([]applyItem{{Kind: "notify", Name: name, YAML: string(y)}}, sec, dry, yes, stdin)
	if err != nil {
		return err
	}
	if c.json() {
		return c.jsonOut(out)
	}
	return nil
}

func (c *cli) notifyTest(name string) error {
	var res struct {
		OK     bool   `json:"ok"`
		Status int    `json:"status"`
		Error  string `json:"error"`
	}
	if err := c.call("POST", "/api/notify/"+url.PathEscape(name)+"/test", nil, &res); err != nil {
		return err
	}
	if c.json() {
		if err := c.jsonOut(res); err != nil {
			return err
		}
	}
	if !res.OK {
		return &client.Error{Msg: "the test message to " + name + " failed: " + res.Error, Hint: "check the url and token, and for a private address that its host:port is in server.services.private_endpoints"}
	}
	c.say("sent (HTTP %d)\n", res.Status)
	return nil
}

func (c *cli) notifyLog(channel string, job int64, limit int) error {
	q := url.Values{}
	if channel != "" {
		q.Set("channel", channel)
	}
	if job > 0 {
		q.Set("job", strconv.FormatInt(job, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	p := "/api/notifications"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var rows []map[string]any
	if err := c.call("GET", p, nil, &rows); err != nil {
		return err
	}
	if c.json() {
		return c.jsonOut(nonNilRows(rows))
	}
	for _, r := range rows {
		if str(r, "sent_at") != "" {
			r["when"] = humanTime(str(r, "sent_at"))
		} else {
			r["when"] = humanTime(str(r, "created_at"))
		}
	}
	return c.table([]string{"ID", "CHANNEL", "EVENT", "JOB", "STATE", "TRIES", "WHEN", "ERROR"}, mapRows(rows, "id", "channel", "event", "job", "state", "attempts", "when", "error"))
}
