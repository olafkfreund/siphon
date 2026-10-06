// Command agentgw is a self-hosted MCP/API gateway: sources -> rules -> actions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/action"
	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/cred"
	"github.com/olafkfreund/MCP-AgentGateway/internal/job"
	"github.com/olafkfreund/MCP-AgentGateway/internal/rule"
	"github.com/olafkfreund/MCP-AgentGateway/internal/source"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
	"github.com/olafkfreund/MCP-AgentGateway/internal/web"
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
  credentials import [-config f] [-token-stdin] <name>   store a login read from stdin
  credentials ls [-config f]              list stored logins (no secrets)
  schema                                  print the JSON Schema for agentgw.yaml
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
	case "exec-job":
		// Internal: `agentgw exec-job <run dir>` runs inside agentgw-action@.service.
		if len(os.Args) != 3 || !filepath.IsAbs(os.Args[2]) {
			fmt.Fprintln(os.Stderr, "usage: agentgw exec-job <absolute run dir>")
			os.Exit(125)
		}
		os.Exit(action.ExecJob(os.Args[2]))
	case "schema":
		var b []byte
		if b, err = config.Schema(); err == nil {
			_, err = os.Stdout.Write(b)
		}
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
	case "credentials":
		err = credentials(args)
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
	p := job.New(cfg, st, time.Now)
	// Bind first so a bad or busy address fails at startup, not silently later.
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		Handler: web.New(web.Options{
			Token: cfg.Server.Token.Value, Store: st, Cfg: cfg, Decide: p.Decide,
			Hooks: p.Webhooks(), Now: time.Now,
		}),
	}
	httpErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed; stopping", "err", err)
			cancel() // never run workers without the HTTP side
		}
		httpErr <- err
	}()
	slog.Info("agentgw serving", "listen", ln.Addr().String(), "workers", cfg.Server.Workers, "sources", len(cfg.Sources))
	serveErr := p.Serve(ctx)
	cancel() // if Serve failed early, take HTTP down too
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	// Shutdown waits for in-flight handlers (webhooks mid-transaction), so the
	// deferred store Close only runs once nothing uses the DB.
	shutErr := srv.Shutdown(sctx)
	if herr := <-httpErr; !errors.Is(herr, http.ErrServerClosed) {
		return errors.Join(serveErr, herr)
	}
	return errors.Join(serveErr, shutErr)
}

func credentials(args []string) error {
	if len(args) == 0 || (args[0] != "import" && args[0] != "ls") {
		return errors.New("usage: agentgw credentials import [-config f] [-token-stdin] <name> | credentials ls [-config f]")
	}
	fs := flag.NewFlagSet("credentials "+args[0], flag.ContinueOnError)
	path := fs.String("config", "agentgw.yaml", "config file")
	token := fs.Bool("token-stdin", false, "stdin is a bare token (claude setup-token)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	st := cred.StoreFor(cfg)
	if args[0] == "ls" {
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPROVIDER\tEXPIRES\tLAST-WRITE")
		names := make([]string, 0, len(cfg.Credentials))
		for n := range cfg.Credentials {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			c := cfg.Credentials[name]
			exp, wrote := "-", "-"
			if c.APIKey.Ref == "" {
				exp, wrote = "not imported", "-"
				if e, w, err := st.Info(name); err == nil {
					exp, wrote = tstr(e), tstr(w)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, c.Provider, exp, wrote)
		}
		return tw.Flush()
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentgw credentials import [-config f] [-token-stdin] <name> < loginfile")
	}
	name := fs.Arg(0)
	c := cfg.Credentials[name]
	if c == nil || c.APIKey.Ref != "" {
		return fmt.Errorf("credential %q is not a subscription credential in the config", name)
	}
	raw, err := cred.ReadLimited(os.Stdin)
	if err != nil {
		return err
	}
	file, b, err := cred.ValidateFor(c.Provider, true, raw)
	if *token {
		file, b, err = cred.Validate(c.Provider, cred.ImportToken, raw)
	}
	if err != nil {
		return err
	}
	if err := st.Put(name, file, b); err != nil {
		return err
	}
	fmt.Printf("imported %s (%s), expires %s\n", name, c.Provider, tstr(cred.Expiry(c.Provider, file, b)))
	return nil
}

func tstr(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}
