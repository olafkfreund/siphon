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
	"syscall"
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

func serve(ctx context.Context, args []string) error {
	cfg, _, err := load("serve", args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	slog.Info("agentgw serving", "workers", cfg.Server.Workers, "sources", len(cfg.Sources))
	return (&job.Pipeline{Cfg: cfg, Store: st, Now: time.Now}).Serve(ctx)
}
