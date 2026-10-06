// Command agentgw is a self-hosted MCP/API gateway: sources -> rules -> actions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/action"
	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/job"
	"github.com/olafkfreund/MCP-AgentGateway/internal/rule"
	"github.com/olafkfreund/MCP-AgentGateway/internal/source"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

var version = "dev"

const usage = `usage: agentgw <command> [flags]

commands:
  validate [-config f]                    check a config file
  rules test [-config f] <rule> <event>   dry-run a rule against a saved event (JSON file)
  run-once [-config f]                    poll every source once and run matching actions
  serve [-config f]                       run the daemon
  jobs ls [-config f] [-state s]          list jobs
  approve|deny [-config f] [-by n] <id>   decide a pending job
  version                                 print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "version":
		fmt.Println(version)
	case "validate":
		err = validate(args)
	case "rules":
		if len(args) == 0 || args[0] != "test" {
			err = errors.New("usage: agentgw rules test [-config f] <rule> <event.json>")
			break
		}
		err = rulesTest(ctx, args[1:])
	case "run-once":
		err = runOnce(ctx, args)
	case "jobs":
		if len(args) == 0 || args[0] != "ls" {
			err = errors.New("usage: agentgw jobs ls [-config f] [-state s]")
			break
		}
		err = jobsLs(args[1:])
	case "approve", "deny":
		err = decide(cmd == "approve", args)
	case "serve":
		err = serve(ctx, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentgw:", err)
		os.Exit(1)
	}
}

// load parses -config from args, loads and validates it; rest are positional args.
func load(name string, args []string) (*config.Config, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "agentgw.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	return cfg, fs.Args(), cfg.Validate()
}

func validate(args []string) error {
	if _, _, err := load("validate", args); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func rulesTest(ctx context.Context, args []string) error {
	cfg, rest, err := load("rules test", args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return errors.New("usage: agentgw rules test [-config f] <rule> <event.json>")
	}
	var r *config.Rule
	for i := range cfg.Rules {
		if cfg.Rules[i].Name == rest[0] {
			r = &cfg.Rules[i]
		}
	}
	if r == nil {
		return fmt.Errorf("unknown rule %q", rest[0])
	}
	b, err := os.ReadFile(rest[1])
	if err != nil {
		return err
	}
	data, err := source.DecodeJSON(b)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		return err
	}
	defer st.Close()
	cfg.Rules = []config.Rule{*r}
	p := &job.Pipeline{Cfg: cfg, Store: st, Now: time.Now}
	fires, _, evalErr := p.HandleEvent(ctx, rule.Event{Source: r.Source, Data: data}, true)

	type out struct {
		Key  string   `json:"key"`
		Item any      `json:"item,omitempty"`
		Argv []string `json:"argv,omitempty"`
		Err  string   `json:"error,omitempty"`
	}
	res := struct {
		Rule  string `json:"rule"`
		Fires []out  `json:"fires"`
	}{Rule: r.Name, Fires: []out{}}
	for _, f := range fires {
		o := out{Key: f.Key, Item: f.Item}
		if len(r.Action.Cmd) > 0 {
			if o.Argv, err = action.Render(r.Action.Cmd, f.Env); err != nil {
				o.Err = err.Error()
			}
		}
		res.Fires = append(res.Fires, o)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	return evalErr
}

func runOnce(ctx context.Context, args []string) error {
	cfg, _, err := load("run-once", args)
	if err != nil {
		return err
	}
	unlock, err := store.Lock(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := (&job.Pipeline{Cfg: cfg, Store: st, Now: time.Now}).RunOnce(ctx); err != nil {
		return errors.New(string(action.Mask([]byte(err.Error()), cfg.Secrets())))
	}
	return nil
}

func jobsLs(args []string) error {
	fs := flag.NewFlagSet("jobs ls", flag.ContinueOnError)
	state := fs.String("state", "", "only jobs in this state")
	st, _, err := openLocal(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	rows, err := store.ListJobs(st.DB, *state)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tRULE\tSTATE\tATTEMPT\tCREATED\tEXIT")
	for _, j := range rows {
		exit := "-"
		if j.ExitCode != nil {
			exit = strconv.Itoa(*j.ExitCode)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\n", j.ID, j.Rule, j.State, j.Attempt, j.CreatedAt.Format(time.RFC3339), exit)
	}
	return w.Flush()
}

func decide(approve bool, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	by := fs.String("by", os.Getenv("USER"), "who is deciding")
	st, rest, err := openLocal(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	if len(rest) != 1 {
		return errors.New("usage: agentgw approve|deny [-config f] [-by name] <job id>")
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return fmt.Errorf("bad job id %q", rest[0])
	}
	if *by == "" {
		*by = "cli"
	}
	return (&job.Pipeline{Store: st, Now: time.Now}).Decide(id, approve, *by)
}

// openLocal adds -config to fs, parses args and opens the DB named by the config.
func openLocal(fs *flag.FlagSet, args []string) (*store.Store, []string, error) {
	path := fs.String("config", "agentgw.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.Server.DB)
	return st, fs.Args(), err
}

func serve(ctx context.Context, args []string) error {
	cfg, _, err := load("serve", args)
	if err != nil {
		return err
	}
	unlock, err := store.Lock(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	slog.Info("agentgw serving", "workers", cfg.Server.Workers, "sources", len(cfg.Sources))
	return (&job.Pipeline{Cfg: cfg, Store: st, Now: time.Now}).Serve(ctx)
}
