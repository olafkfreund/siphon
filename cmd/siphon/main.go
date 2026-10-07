// Command siphon is a self-hosted MCP/API gateway: sources -> rules -> actions.
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/agentloop"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/cred"
	"github.com/olafkfreund/siphon/internal/job"
	"github.com/olafkfreund/siphon/internal/mcpbridge"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
	"github.com/olafkfreund/siphon/internal/web"
)

var version = "dev"

const usage = `usage: siphon <command> [flags]

commands:
  validate [-config f] [-v] [-file-only]  check the config (file + portal edits; -file-only: file alone; -v: print egress allowlists)
  config export [-config f]               print the effective YAML (file + portal edits)
  rules test [-config f] <rule> <event>   dry-run a rule against a saved event (JSON file)
  run-once [-config f]                    poll every source once and run matching actions
  serve [-config f]                       run the daemon
  jobs ls [-config f] [-state s]          list jobs
  approve|deny [-config f] [-by n] <id>   decide a pending job
  credentials import [-config f] [-token-stdin] <name>   store a login read from stdin
  credentials ls [-config f]              list stored logins (no secrets)
  schema                                  print the JSON Schema for siphon.yaml
  version                                 print the version
`

func main() {
	if filepath.Base(os.Args[0]) == "agentgw" { // legacy-name
		fmt.Fprintln(os.Stderr, "agentgw is now siphon; this alias goes away in v0.2.0") // legacy-name
	}
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
		// Internal: `siphon exec-job <run dir>` runs inside siphon-action@.service.
		if len(os.Args) != 3 || !filepath.IsAbs(os.Args[2]) {
			fmt.Fprintln(os.Stderr, "usage: siphon exec-job <absolute run dir>")
			os.Exit(125)
		}
		os.Exit(action.ExecJob(os.Args[2]))
	case "agent-run":
		// Internal: `siphon agent-run <spec.json>` is the model agent, run by the action sandbox.
		var spec agentloop.Spec
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: siphon agent-run <spec.json>")
			os.Exit(2)
		}
		b, rerr := os.ReadFile(args[0])
		if rerr == nil {
			rerr = json.Unmarshal(b, &spec)
		}
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "agent-run:", rerr)
			os.Exit(2)
		}
		os.Exit(agentloop.Run(ctx, spec, os.Stdout))
	case "mcp-bridge":
		// Internal: `siphon mcp-bridge <spec.json>` runs inside siphon-mcp@.service.
		var spec mcpbridge.Spec
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: siphon mcp-bridge <spec.json>")
			os.Exit(2)
		}
		b, rerr := os.ReadFile(args[0])
		if rerr == nil {
			rerr = json.Unmarshal(b, &spec)
		}
		if rerr == nil {
			rerr = mcpbridge.Run(ctx, spec)
		}
		if rerr != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "mcp-bridge:", rerr)
			os.Exit(1)
		}
		os.Exit(0)
	case "schema":
		var b []byte
		if b, err = config.Schema(); err == nil {
			_, err = os.Stdout.Write(b)
		}
	case "validate":
		err = validate(args)
	case "config":
		if len(args) == 0 || args[0] != "export" {
			err = errors.New("usage: siphon config export [-config f]")
			break
		}
		err = configExport(args[1:])
	case "rules":
		if len(args) == 0 || args[0] != "test" {
			err = errors.New("usage: siphon rules test [-config f] <rule> <event.json>")
			break
		}
		err = rulesTest(ctx, args[1:])
	case "run-once":
		err = runOnce(ctx, args)
	case "jobs":
		if len(args) == 0 || args[0] != "ls" {
			err = errors.New("usage: siphon jobs ls [-config f] [-state s]")
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
		fmt.Fprintln(os.Stderr, "siphon:", err)
		os.Exit(1)
	}
}

// resolveConfig returns path, except that when -config wasn't given and only the
// old default file exists it returns that one, with a warning.
func resolveConfig(fs *flag.FlagSet, path string) string {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "config" })
	if set {
		return path
	}
	if _, err := os.Stat(path); err != nil {
		if _, err := os.Stat("agentgw.yaml"); err == nil { // legacy-name
			fmt.Fprintln(os.Stderr, "warning: agentgw.yaml is deprecated, rename it to siphon.yaml") // legacy-name
			return "agentgw.yaml"                                                                    // legacy-name
		}
	}
	return path
}

// load parses -config from args, loads and validates it (file plus portal
// edits unless -file-only); rest are positional args.
func load(name string, args []string) (*config.Config, []string, error) {
	cfg, rest, _, _, err := loadCfg(name, args, false)
	return cfg, rest, err
}

// loadCfg is load, also returning the resolved config path; with fallback, an overlay that makes the config invalid is
// replaced by the last valid revision (or the file alone) and banner says so.
func loadCfg(name string, args []string, fallback bool) (*config.Config, []string, string, string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "siphon.yaml", "config file")
	fileOnly := fs.Bool("file-only", false, "ignore portal edits")
	if err := fs.Parse(args); err != nil {
		return nil, nil, "", "", err
	}
	p := resolveConfig(fs, *path)
	var items []config.Item
	if !*fileOnly {
		base, err := config.Load(p)
		if err != nil {
			return nil, nil, "", "", err
		}
		if items, err = dbOverlay(base.Server.DB); err != nil {
			return nil, nil, "", "", err
		}
	}
	var cfg *config.Config
	var banner string
	var err error
	if fallback {
		cfg, banner, err = pickConfig(p, items)
	} else {
		cfg, err = tryLoad(p, items)
	}
	if cfg == nil {
		return nil, nil, "", "", err
	}
	applyListenEnv(cfg) // before Warnings, which judge the listen address
	for _, w := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	return cfg, fs.Args(), banner, p, err
}

// tryLoad loads the file plus items and validates; the config is returned
// with a Validate error so `validate` can still list warnings first.
func tryLoad(path string, items []config.Item) (*config.Config, error) {
	cfg, _, err := config.LoadWithOverlay(path, items)
	if err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// pickConfig is serve's startup decision: file + items, else the file with
// only the portal's deletions applied (so what an operator removed stays removed).
// banner is set on a fallback.
func pickConfig(path string, items []config.Item) (*config.Config, string, error) {
	cfg, err := tryLoad(path, items)
	if err == nil || len(items) == 0 {
		return cfg, "", err
	}
	var tombs []config.Item
	for _, it := range items {
		if it.Deleted {
			tombs = append(tombs, it)
		}
	}
	fb, ferr := tryLoad(path, tombs)
	if ferr != nil {
		return nil, "", err
	}
	slog.Error("portal edits could not be applied", "err", err)
	return fb, fmt.Sprintf("Portal edits could not be applied: %v; running on the file with its deletions only", err), nil
}

// dbOverlay reads the portal edits from the DB,
// if it exists (it is never created here).
func dbOverlay(dbPath string) (items []config.Item, err error) {
	if dbPath == ":memory:" {
		return nil, nil
	}
	if _, serr := os.Stat(dbPath); serr != nil {
		return nil, nil
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	cis, err := store.ConfigItems(st.DB)
	if err != nil {
		return nil, err
	}
	for _, i := range cis {
		items = append(items, config.Item{Kind: i.Kind, Name: i.Name, YAML: i.YAML, Deleted: i.Deleted})
	}
	return items, nil
}

// configExport prints the effective YAML: the file with the portal edits applied.
func configExport(args []string) error {
	fs := flag.NewFlagSet("config export", flag.ContinueOnError)
	path := fs.String("config", "siphon.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := resolveConfig(fs, *path)
	base, err := config.Load(p)
	if err != nil {
		return err
	}
	items, err := dbOverlay(base.Server.DB)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	out, _, err := config.Effective(b, items)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

func validate(args []string) error {
	verbose := slices.Contains(args, "-v")
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "-v" })
	cfg, _, err := load("validate", args)
	if err != nil {
		return err
	}
	if verbose {
		printEgress(cfg)
	}
	fmt.Println("ok")
	return nil
}

func printEgress(cfg *config.Config) {
	list := func(a []config.HostPort) string {
		s := make([]string, len(a))
		for i, h := range a {
			s[i] = h.String()
		}
		return strings.Join(s, ", ")
	}
	names := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if a, on := cfg.AgentEgress(cfg.Agents[n]); on {
			fmt.Printf("agent %s egress: %s\n", n, list(a))
		} else {
			fmt.Printf("agent %s egress: off\n", n)
		}
	}
	for _, r := range cfg.Rules {
		if a, on := cfg.RuleEgress(r); on {
			fmt.Printf("rule %s egress: %s\n", r.Name, list(a))
		}
	}
}

func rulesTest(ctx context.Context, args []string) error {
	cfg, rest, err := load("rules test", args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return errors.New("usage: siphon rules test [-config f] <rule> <event.json>")
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
	// {"headers": {...}, "event": {...}} is an envelope; anything else is the event itself.
	var headers map[string]string
	if m, ok := data.(map[string]any); ok && len(m) == 2 && m["event"] != nil && m["headers"] != nil {
		hm, ok := m["headers"].(map[string]any)
		if !ok {
			return errors.New("event: headers must be an object of strings")
		}
		headers = make(map[string]string, len(hm))
		for k, v := range hm {
			sv, ok := v.(string)
			if !ok {
				return fmt.Errorf("event: header %q must be a string", k)
			}
			headers[strings.ToLower(k)] = sv
		}
		data = m["event"]
	}
	fires, evalErr := rule.DryRun(ctx, *r, rule.Event{Source: r.Source, Headers: headers, Data: data})

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
	if err := job.New(cfg, st, time.Now).RunOnce(ctx); err != nil {
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
		return errors.New("usage: siphon approve|deny [-config f] [-by name] <job id>")
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
	path := fs.String("config", "siphon.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(resolveConfig(fs, *path))
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.Server.DB)
	return st, fs.Args(), err
}

// applyListenEnv lets SIPHON_LISTEN override server.listen (the image and microVM set it).
func applyListenEnv(cfg *config.Config) {
	if l := os.Getenv("SIPHON_LISTEN"); l != "" {
		cfg.Server.Listen = l
		slog.Info("listen overridden by SIPHON_LISTEN", "listen", l)
	}
}

func serve(ctx context.Context, args []string) error {
	cfg, _, banner, cfgPath, err := loadCfg("serve", args, true)
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
	if cfg.Server.Sandbox == "none" && config.InContainer() {
		slog.Warn("running in a container with sandbox: none: runs are not isolated and egress is not enforced")
	}
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
			Token: cfg.Server.Token.Value, Store: st, Config: p.Config, Apply: p.Apply, Banner: banner, Unsandboxed: cfg.Server.Sandbox == "none", ConfigPath: cfgPath, Decide: p.Decide, TestAWS: p.TestAWS,
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
	slog.Info("siphon serving", "listen", ln.Addr().String(), "workers", cfg.Server.Workers, "sources", len(cfg.Sources))
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
		return errors.New("usage: siphon credentials import [-config f] [-token-stdin] <name> | credentials ls [-config f]")
	}
	fs := flag.NewFlagSet("credentials "+args[0], flag.ContinueOnError)
	path := fs.String("config", "siphon.yaml", "config file")
	token := fs.Bool("token-stdin", false, "stdin is a bare token (claude setup-token)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(resolveConfig(fs, *path))
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
			if c.IsModel() { // a model endpoint: nothing to import
				exp = "endpoint " + c.URL
			} else if c.APIKey.Ref == "" {
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
		return errors.New("usage: siphon credentials import [-config f] [-token-stdin] <name> < loginfile")
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
	file, b, err := cred.ValidateImport(c.Provider, *token, raw)
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
