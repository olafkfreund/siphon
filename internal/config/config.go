// Package config loads and validates siphon.yaml, the only source of truth.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/expr-lang/expr"
	"gopkg.in/yaml.v3"
)

// AgentResultSource is the built-in source for agent JSON results (loop guard).
const AgentResultSource = "agent-result"

// MaxDepth caps agent-result chains (loop guard): an event at a deeper depth never enqueues.
const MaxDepth = 2

var (
	envName   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	envDenied = regexp.MustCompile(`^(LD_.*|DYLD_.*|PATH|HOME|NODE_OPTIONS|PYTHON.*|BASH_ENV|ENV|PERL5.*|RUBY.*|JAVA_TOOL_OPTIONS|SSL_CERT_.*|GIT_.*|.*_PROXY)$`)
)

var safePath = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

// Duration unmarshals from strings like "5m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// ByteSize unmarshals from "1MiB", "512KiB", "100" (bytes), etc.
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	s := strings.TrimSpace(n.Value)
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"B", 1}} { // "B" last
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suf)), u.m
			break
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: invalid size %q", n.Line, n.Value)
	}
	*b = ByteSize(v * mult)
	return nil
}

// Secret is an `env:NAME` or `file:/path` reference. Ref is what the YAML
// said; Value is filled in by Load.
type Secret struct{ Ref, Value string }

func (s *Secret) UnmarshalYAML(n *yaml.Node) error { s.Ref = n.Value; return nil }

func (s Secret) isSet() bool { return s.Ref != "" }

type Config struct {
	Server  Server             `yaml:"server"`
	Limits  Limits             `yaml:"limits"`
	Sources map[string]*Source `yaml:"sources"`
	Rules   []Rule             `yaml:"rules"`
	Agents  map[string]*Agent  `yaml:"agents"`
	// Credentials are logins siphon owns: subscription (store files) or, with api_key, an API key.
	Credentials map[string]*Credential `yaml:"credentials"`
	Routines    map[string]*Routine    `yaml:"routines"`
	Units       []string               `yaml:"units"`

	resolveErrs []error
	legacyDB    string // set when the default db fell back to an old agentgw.db // legacy-name
}

type Server struct {
	Listen  string `yaml:"listen"`
	DB      string `yaml:"db"`
	Workers int    `yaml:"workers"`
	Token   Secret `yaml:"token"`
	Sandbox string `yaml:"sandbox"` // systemd|none
	// ActionsDir holds per-run directories for sandboxed actions; the NixOS
	// module sets it to the setgid siphon-io directory its template unit uses.
	ActionsDir string       `yaml:"actions_dir"`
	Egress     EgressServer `yaml:"egress"`
	Models     ModelsServer `yaml:"models"`
}

// ModelsServer holds model-endpoint settings that only siphon.yaml may set.
type ModelsServer struct {
	// PrivateEndpoints are the host:port model endpoints allowed to be on a
	// private, loopback or LAN address (never link-local or metadata).
	PrivateEndpoints []string `yaml:"private_endpoints"`
}

// EgressServer configures the egress proxy that restricts sandboxed runs.
type EgressServer struct {
	Listen     string   `yaml:"listen"` // loopback ip:port
	Allow      []string `yaml:"allow"`  // global extra hosts: host, host:port or *.suffix[:port]
	CmdDefault bool     `yaml:"cmd_default"`
	Socket     string   `yaml:"socket"` // optional unix socket path (absolute)
}

// EgressAgent is an agent's egress setting; Enabled defaults to true.
type EgressAgent struct {
	Enabled *bool    `yaml:"enabled"`
	Allow   []string `yaml:"allow"`
}

// EgressRule opts a rule's cmd actions in to egress restriction.
type EgressRule struct {
	Enabled bool     `yaml:"enabled"`
	Allow   []string `yaml:"allow"`
}

type Limits struct {
	AgentRunsPerDay int      `yaml:"agent_runs_per_day"`
	HTTPMaxBody     ByteSize `yaml:"http_max_body"`
	HTTPTimeout     Duration `yaml:"http_timeout"`
}

type Source struct {
	Type         string            `yaml:"type"` // mcp|http|webhook
	URL          string            `yaml:"url"`
	Command      []string          `yaml:"command"`
	Read         *Read             `yaml:"read"`
	Poll         Duration          `yaml:"poll"`
	Auth         *Auth             `yaml:"auth"`
	AllowPrivate bool              `yaml:"allow_private"`
	Method       string            `yaml:"method"`  // http: GET (default) or POST
	Headers      map[string]Secret `yaml:"headers"` // values may be env:/file: refs or plain literals
	Body         string            `yaml:"body"`
	Secret       Secret            `yaml:"secret"`           // webhook HMAC key
	Signature    string            `yaml:"signature"`        // github|sha256|token|standard-webhooks
	SigHeader    string            `yaml:"signature_header"` // sha256 preset
	TokenHeader  string            `yaml:"token_header"`     // token preset
	TimestampHdr string            `yaml:"timestamp_header"`
	ID           string            `yaml:"id"`  // delivery id, e.g. header.X-GitHub-Delivery
	Env          map[string]Secret `yaml:"env"` // stdio MCP child env; values are env:/file: refs
}

type Read struct {
	Resource string         `yaml:"resource"`
	Tool     string         `yaml:"tool"`
	Args     map[string]any `yaml:"args"`
}

type Auth struct {
	Bearer Secret `yaml:"bearer"`
}

type Rule struct {
	Name             string     `yaml:"name"`
	Source           string     `yaml:"source"`
	ForEach          string     `yaml:"for_each"`
	ID               string     `yaml:"id"`
	When             string     `yaml:"when"`
	On               string     `yaml:"on"` // edge (default)|each
	Repeat           Duration   `yaml:"repeat"`
	Cooldown         Duration   `yaml:"cooldown"`
	AllowAgentEvents bool       `yaml:"allow_agent_events"`
	Action           Action     `yaml:"action"`
	Approve          bool       `yaml:"approve"`
	Egress           EgressRule `yaml:"egress"`
}

type Action struct {
	Cmd     []string `yaml:"cmd"`
	Unit    string   `yaml:"unit"`
	Agent   string   `yaml:"agent"`
	Routine string   `yaml:"routine"`
}

// Credential is a subscription login (no api_key) or an API key for one provider.
type Credential struct {
	Provider    string `yaml:"provider"` // claude|codex|agy|ollama|openai
	URL         string `yaml:"url"`      // ollama|openai: http(s)://host[:port][/path]
	Preset      string `yaml:"preset"`   // openai: UI tile only
	APIKey      Secret `yaml:"api_key"`
	Concurrency int    `yaml:"concurrency"` // parallel jobs on this login, default 1
}

const implicitPrefix = "_apikey_" // credentials made from api_key_file

var credName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var kinds = []string{"claude", "codex", "agy"}

// modelProviders are the credential providers that point at a model endpoint.
var modelProviders = []string{"ollama", "openai"}

type Agent struct {
	Egress     EgressAgent `yaml:"egress"`
	Kind       string      `yaml:"kind"` // claude (default)|codex|agy|model
	Credential string      `yaml:"credential"`
	Model      string      `yaml:"model"`   // kind model: the model id
	Command    string      `yaml:"command"` // binary path override
	// Deprecated: use kind and command.
	Runner       []string `yaml:"runner"`
	Prompt       string   `yaml:"prompt"`
	MCP          []string `yaml:"mcp"`
	AllowedTools []string `yaml:"allowed_tools"`
	MaxTurns     int      `yaml:"max_turns"`
	MaxBudgetUSD float64  `yaml:"max_budget_usd"`
	Timeout      Duration `yaml:"timeout"`
	Approve      *bool    `yaml:"approve"` // default true
	// APIKeyFile is passed to the runner as a systemd credential and read by
	// claude's apiKeyHelper (--bare only reads ANTHROPIC_API_KEY or apiKeyHelper).
	APIKeyFile string `yaml:"api_key_file"`
}

type Routine struct {
	Steps []Step `yaml:"steps"`
}

type Step struct {
	ID              string   `yaml:"id"`
	Cmd             []string `yaml:"cmd"`
	Unit            string   `yaml:"unit"`
	Agent           string   `yaml:"agent"`
	If              string   `yaml:"if"`
	Retry           *Retry   `yaml:"retry"`
	Timeout         Duration `yaml:"timeout"`
	ContinueOnError bool     `yaml:"continue_on_error"`
	Approve         bool     `yaml:"approve"`
}

// Retry re-runs a failed step. Attempts is the total number of tries, so 0 or 1
// means no retry. Waits grow as Base*Factor^n with jitter (defaults 10s and 2).
type Retry struct {
	Attempts int      `yaml:"attempts"`
	Base     Duration `yaml:"base"`
	Factor   float64  `yaml:"factor"`
}

const MaxRetryAttempts = 10

// NeedsApproval reports whether a job from r waits for a person: the rule says
// so, or its agent does.
func (c *Config) NeedsApproval(r Rule) bool {
	if r.Approve {
		return true
	}
	if a := c.Agents[r.Action.Agent]; a != nil && a.Approve != nil {
		return *a.Approve
	}
	return false
}

// Load reads path, applies defaults and resolves secret refs. Unresolvable
// refs are reported by Validate, so `validate` lists every problem at once.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, err
	}
	c.resolveDB(path)
	return c, nil
}

// resolveDB points a relative db at the config file's directory and applies
// the legacy-name fallback.
func (c *Config) resolveDB(path string) {
	// A relative db lives next to the config file, wherever the daemon is started.
	if d := c.Server.DB; d != "" && d != ":memory:" && !filepath.IsAbs(d) {
		c.Server.DB = filepath.Join(filepath.Dir(path), d)
	}
	// The default db was agentgw.db before the rename: keep using an existing one. // legacy-name
	if filepath.Base(c.Server.DB) == "siphon.db" {
		old := filepath.Join(filepath.Dir(c.Server.DB), "agentgw.db") // legacy-name
		if _, err := os.Stat(c.Server.DB); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(old); err == nil {
				c.Server.DB, c.legacyDB = old, old
			}
		}
	}
}

func Parse(b []byte) (*Config, error) { return parse(b, nil) }

// parse is Parse; stub maps a secret ref to a stand-in value, so a candidate
// whose secret files are not written yet can still be validated.
func parse(b []byte, stub map[string]string) (*Config, error) {
	c := &Config{
		Server: Server{Listen: ":8080", DB: "siphon.db", Workers: 4, Sandbox: "systemd", Egress: EgressServer{Listen: "127.77.0.1:3128"}},
		Limits: Limits{AgentRunsPerDay: 50, HTTPMaxBody: 1 << 20, HTTPTimeout: Duration(30 * time.Second)},
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse config: multiple YAML documents are not supported")
	}
	for _, s := range c.Sources {
		if s != nil && s.Poll == 0 && s.Type != "webhook" {
			s.Poll = Duration(time.Minute)
		}
	}
	for _, a := range c.Agents {
		if a == nil {
			continue
		}
		if a.Kind == "" {
			a.Kind = "claude"
		}
		if a.Egress.Enabled == nil {
			t := true
			a.Egress.Enabled = &t
		}
		if len(a.Runner) > 0 && a.Command == "" {
			a.Command = a.Runner[0]
		}
		if a.Approve == nil {
			t := true
			a.Approve = &t
		}
		if a.Timeout == 0 {
			a.Timeout = Duration(10 * time.Minute)
		}
	}
	for _, cr := range c.Credentials {
		if cr != nil && cr.Concurrency == 0 {
			cr.Concurrency = 1
		}
	}
	for _, name := range sortedKeys(c.Agents) {
		a := c.Agents[name]
		if a == nil || a.Credential != "" {
			continue
		}
		if a.APIKeyFile != "" {
			if c.Credentials == nil {
				c.Credentials = map[string]*Credential{}
			}
			a.Credential = implicitPrefix + name
			if c.Credentials[a.Credential] != nil {
				c.resolveErrs = append(c.resolveErrs, fmt.Errorf("agents.%s: implicit credential name %q collides with a declared credential", name, a.Credential))
				continue
			}
			c.Credentials[a.Credential] = &Credential{Provider: a.Kind, APIKey: Secret{Ref: "file:" + a.APIKeyFile}, Concurrency: 1}
			continue
		}
		var only []string
		for _, cn := range sortedKeys(c.Credentials) {
			if cr := c.Credentials[cn]; cr != nil && cr.Provider == a.Kind && !cr.APIKey.isSet() {
				only = append(only, cn)
			}
		}
		if len(only) == 1 {
			a.Credential = only[0]
		}
	}
	for _, s := range c.secretPtrs() {
		if err := s.resolve(stub); err != nil {
			c.resolveErrs = append(c.resolveErrs, err)
		}
	}
	for _, name := range sortedKeys(c.Sources) {
		if src := c.Sources[name]; src != nil {
			for _, h := range sortedKeys(src.Headers) {
				v := src.Headers[h]
				if err := v.resolve(stub); err != nil {
					c.resolveErrs = append(c.resolveErrs, err)
				}
				src.Headers[h] = v
			}
			for _, k := range sortedKeys(src.Env) {
				v := src.Env[k]
				if err := v.resolve(stub); err != nil {
					c.resolveErrs = append(c.resolveErrs, err)
				}
				src.Env[k] = v
			}
		}
	}
	return c, nil
}

// secretPtrs are values that must always be env:/file: refs. Source headers
// are handled separately: they may be plain literals unless the name looks secret.
func (c *Config) secretPtrs() []*Secret {
	out := []*Secret{&c.Server.Token}
	for _, name := range sortedKeys(c.Credentials) {
		if cr := c.Credentials[name]; cr != nil {
			out = append(out, &cr.APIKey)
		}
	}
	for _, name := range sortedKeys(c.Sources) {
		if s := c.Sources[name]; s != nil {
			out = append(out, &s.Secret)
			if s.Auth != nil {
				out = append(out, &s.Auth.Bearer)
			}
		}
	}
	return out
}

func (s *Secret) resolve(stub map[string]string) error {
	if v, ok := stub[s.Ref]; ok && s.Ref != "" {
		s.Value = v
		return nil
	}
	switch {
	case strings.HasPrefix(s.Ref, "env:"):
		name := s.Ref[4:]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return fmt.Errorf("secret %s: environment variable is unset or empty", s.Ref)
		}
		s.Value = v
	case strings.HasPrefix(s.Ref, "file:"):
		b, err := os.ReadFile(s.Ref[5:])
		if err != nil { // no OS error text: it would tell a probing editor whether a path exists
			return fmt.Errorf("secret %s: the file cannot be read", s.Ref)
		}
		s.Value = strings.TrimSpace(string(b))
		if s.Value == "" {
			return fmt.Errorf("secret %s: file is empty", s.Ref)
		}
	}
	return nil
}

// Secrets returns the resolved secret values, for masking stored output and logs.
func (c *Config) Secrets() []string {
	var out []string
	for _, s := range c.secretPtrs() {
		if s.Value != "" {
			out = append(out, s.Value)
		}
	}
	for _, name := range sortedKeys(c.Sources) { // inline literal headers have no Value
		if src := c.Sources[name]; src != nil {
			for _, h := range sortedKeys(src.Headers) {
				if v := src.Headers[h].Value; v != "" {
					out = append(out, v)
				}
			}
			for _, k := range sortedKeys(src.Env) {
				if v := src.Env[k].Value; v != "" {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// Warnings lists non-fatal findings.
func (c *Config) Warnings() []string {
	var w []string
	if c.legacyDB != "" {
		w = append(w, fmt.Sprintf("using %s: rename it to siphon.db (the old default name goes away in v0.2.0)", c.legacyDB)) // legacy-name
	}
	if c.Server.Sandbox == "none" {
		w = append(w, "server.sandbox is none: actions run unsandboxed")
		if c.Server.Egress.CmdDefault {
			w = append(w, "server.egress.cmd_default: egress allowlists are not enforced with sandbox: none")
		}
		for _, r := range c.Rules {
			if r.Egress.Enabled {
				w = append(w, fmt.Sprintf("rule %s: egress allowlists are not enforced with sandbox: none", r.Name))
			}
		}
	}
	if !loopbackListen(c.Server.Listen) {
		w = append(w, fmt.Sprintf("server.listen %q is not loopback: the portal is plain HTTP, put it behind TLS or a reverse proxy", c.Server.Listen))
	}
	for _, name := range sortedKeys(c.Sources) {
		if s := c.Sources[name]; s != nil && s.AllowPrivate {
			w = append(w, fmt.Sprintf("source %s: allow_private disables the private-address guard", name))
		}
		if s := c.Sources[name]; s != nil && s.Type == "webhook" && s.Signature == "token" {
			w = append(w, fmt.Sprintf("source %s: signature token sends the secret in a header: weaker than an HMAC, and a captured delivery can be replayed", name))
		}
		if s := c.Sources[name]; s != nil && s.Auth != nil && s.Auth.Bearer.isSet() && cleartextRemote(s.URL) {
			w = append(w, fmt.Sprintf("source %s: bearer token sent over plain http to a non-loopback host", name))
		}
	}
	for _, name := range sortedKeys(c.Agents) {
		a := c.Agents[name]
		if a == nil {
			continue
		}
		if len(a.Runner) > 0 {
			w = append(w, fmt.Sprintf("agent %s: runner is deprecated, use kind and command", name))
			if len(a.Runner) > 1 {
				w = append(w, fmt.Sprintf("agent %s: runner arguments after the binary are ignored", name))
			}
		}
		if c.Server.Sandbox == "none" && *a.Egress.Enabled {
			w = append(w, fmt.Sprintf("agent %s: egress allowlists are not enforced with sandbox: none", name))
		}
		add := func(f string, a ...any) { w = append(w, fmt.Sprintf("agent %s: ", name)+fmt.Sprintf(f, a...)) }
		if a.Kind == "codex" || a.Kind == "agy" {
			add("built-in shell tools can't be disabled (%s runs %s)", a.Kind, map[string]string{"codex": "read-only", "agy": "in a sandbox in plan mode"}[a.Kind])
		}
		if a.Kind == "agy" && len(a.MCP) > 0 {
			add("tool allowlist not enforced (only MCP server scoping)")
		}
		if a.Kind != "claude" && a.Kind != "model" && a.MaxTurns > 0 { // the built-in loop enforces turns
			add("max_turns not enforced")
		}
		if a.Kind == "model" && a.MaxBudgetUSD > 0 {
			add("max_budget_usd applies only when the endpoint reports a cost")
		} else if a.Kind != "claude" && a.MaxBudgetUSD > 0 {
			add("max_budget_usd not enforced")
		}
	}
	return w
}

// declaredCredentials counts credentials from the credentials: block, not api_key_file ones.
func (c *Config) declaredCredentials() int {
	n := 0
	for name := range c.Credentials {
		if !strings.HasPrefix(name, implicitPrefix) {
			n++
		}
	}
	return n
}

// AgentCredential returns the agent's credential name and entry, or "", nil.
func (c *Config) AgentCredential(agent string) (string, *Credential) {
	a := c.Agents[agent]
	if a == nil || a.Credential == "" {
		return "", nil
	}
	return a.Credential, c.Credentials[a.Credential]
}

func loopbackListen(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil || h == "" {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return h == "localhost"
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9@._:-]+\.(service|target|timer)$`)

var webhookID = regexp.MustCompile(`^header\.[A-Za-z0-9-]+$`)

func cleartextRemote(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); ip != nil {
		return !ip.IsLoopback()
	}
	return h != "localhost"
}

// secretHeader reports whether a header name implies a secret value.
func secretHeader(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"authorization", "cookie", "token", "key", "secret", "password"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// Validate returns every problem at once, joined.
func (c *Config) Validate() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	errs = append(errs, c.resolveErrs...)

	if c.Server.Sandbox != "systemd" && c.Server.Sandbox != "none" {
		add("server.sandbox: must be systemd or none, got %q", c.Server.Sandbox)
	}
	if c.Server.Workers < 1 {
		add("server.workers: must be >= 1")
	}
	for _, s := range c.secretPtrs() {
		if s.isSet() && !strings.HasPrefix(s.Ref, "env:") && !strings.HasPrefix(s.Ref, "file:") {
			add("inline secret: values must be env:NAME or file:/path")
		}
	}

	for _, name := range sortedKeys(c.Sources) {
		if src := c.Sources[name]; src != nil {
			for _, h := range sortedKeys(src.Headers) {
				ref := src.Headers[h].Ref
				if secretHeader(h) && !strings.HasPrefix(ref, "env:") && !strings.HasPrefix(ref, "file:") {
					add("sources.%s.headers.%s: inline secret: values must be env:NAME or file:/path", name, h)
				}
			}
		}
	}

	for _, name := range sortedKeys(c.Sources) {
		if src := c.Sources[name]; src != nil {
			for _, k := range sortedKeys(src.Env) {
				if ref := src.Env[k].Ref; !strings.HasPrefix(ref, "env:") && !strings.HasPrefix(ref, "file:") {
					add("sources.%s.env.%s: inline secret: values must be env:NAME or file:/path", name, k)
				}
			}
		}
	}

	if t := c.Server.Token; t.isSet() && t.Value != "" && len(t.Value) < 32 {
		add("server.token: must be at least 32 characters")
	}
	// IPv4 only: the NixOS module derives the sandbox's IPAddressAllow=<ip>/32 from it.
	if h, _, err := net.SplitHostPort(c.Server.Egress.Listen); err != nil || net.ParseIP(h).To4() == nil || !net.ParseIP(h).IsLoopback() {
		add("server.egress.listen: must be an IPv4 loopback ip:port like 127.77.0.1:3128, got %q", c.Server.Egress.Listen)
	}
	if sock := c.Server.Egress.Socket; sock != "" && !filepath.IsAbs(sock) {
		add("server.egress.socket: must be empty or an absolute path, got %q", sock)
	}
	checkAllow := func(p string, list []string) {
		for _, e := range list {
			if _, err := parseHostPort(e); err != nil {
				add("%s.egress.allow: %v", p, err)
			}
		}
	}
	checkAllow("server", c.Server.Egress.Allow)
	for _, e := range c.Server.Models.PrivateEndpoints {
		if _, _, ok := endpointKey(e); !ok {
			add("server.models.private_endpoints: %q: want host:port", e)
		}
	}
	if c.Limits.HTTPTimeout < 0 {
		add("limits.http_timeout: must not be negative")
	}

	for _, u := range c.Units {
		if strings.Contains(u, "{{") {
			add("units: %q must not be templated", u)
		} else if !unitName.MatchString(u) {
			add("units: %q must be a full unit name like foo.service", u)
		}
	}

	for _, name := range sortedKeys(c.Sources) {
		c.validateSource(name, c.Sources[name], add)
	}

	seen := map[string]bool{}
	for i, r := range c.Rules {
		p := fmt.Sprintf("rules[%d] %q", i, r.Name)
		if r.Name == "" {
			add("rules[%d]: name is required", i)
		} else if seen[r.Name] {
			add("%s: duplicate rule name", p)
		}
		seen[r.Name] = true
		if _, ok := c.Sources[r.Source]; !ok && r.Source != AgentResultSource {
			add("%s: unknown source %q", p, r.Source)
		}
		if r.When == "" {
			add("%s: when is required", p)
		}
		for field, src := range map[string]string{"when": r.When, "id": r.ID, "for_each": r.ForEach} {
			checkExpr(p+" "+field, src, add)
		}
		switch r.On {
		case "", "edge":
		case "each":
			if r.ID == "" {
				add("%s: on: each requires id", p)
			}
		default:
			add("%s: on must be edge or each, got %q", p, r.On)
		}
		if r.Repeat < 0 || r.Cooldown < 0 {
			add("%s: repeat and cooldown must not be negative", p)
		}
		checkAllow(fmt.Sprintf("rules[%d]", i), r.Egress.Allow)
		c.validateAction(p, r.Action, add)
		if (r.Action.Agent != "" || (r.Action.Routine != "" && c.RoutineHasAgent(r.Action.Routine))) && r.Cooldown <= 0 {
			add("%s: cooldown is mandatory for agent actions", p)
		}
	}

	for _, name := range sortedKeys(c.Credentials) {
		cr := c.Credentials[name]
		p := "credentials." + name
		if !credName.MatchString(name) && !strings.HasPrefix(name, implicitPrefix) {
			add("%s: name must match [a-z0-9][a-z0-9_-]*", p)
		}
		if strings.HasPrefix(name, implicitPrefix) && !implicitFor(c, name) {
			add("%s: names starting with %s are reserved", p, implicitPrefix)
		}
		switch {
		case cr == nil:
			add("%s: empty", p)
		case !slices.Contains(kinds, cr.Provider) && !slices.Contains(modelProviders, cr.Provider):
			add("%s: provider must be claude, codex, agy, ollama or openai, got %q", p, cr.Provider)
		case cr.Concurrency < 1:
			add("%s: concurrency must be >= 1", p)
		}
		if cr != nil && slices.Contains(modelProviders, cr.Provider) {
			if _, _, err := ModelURL(cr.URL); err != nil {
				add("%s: url: %v", p, err)
			} else if u, _ := url.Parse(cr.URL); cr.Provider == "ollama" && strings.Trim(u.Path, "/") != "" {
				add("%s: url: ollama takes no path (the loop adds /v1)", p)
			} else if p2 := strings.TrimRight(u.Path, "/"); cr.Provider == "openai" && p2 != "" && !strings.HasSuffix(p2, "/v1") {
				add("%s: url: path must be empty or end in /v1", p)
			}
			if cr.Provider == "ollama" && cr.APIKey.isSet() {
				add("%s: api_key is not allowed for ollama", p)
			}
		} else if cr != nil && cr.URL != "" {
			add("%s: url is only for ollama and openai", p)
		}
		if cr != nil && cr.APIKey.isSet() && !strings.HasPrefix(cr.APIKey.Ref, "env:") && !strings.HasPrefix(cr.APIKey.Ref, "file:") {
			add("%s: api_key must be env:NAME or file:/path", p)
		}
	}

	for _, name := range sortedKeys(c.Agents) {
		a := c.Agents[name]
		p := "agents." + name
		if a == nil {
			add("%s: empty", p)
			continue
		}
		for _, m := range a.MCP {
			if src, ok := c.Sources[m]; !ok {
				add("%s: mcp references unknown source %q", p, m)
			} else if src != nil && src.Type != "mcp" {
				add("%s: mcp source %q has type %s, want mcp", p, m, src.Type)
			}
		}
		if a.Timeout < 0 {
			add("%s: timeout must not be negative", p)
		}
		if len(a.Runner) > 0 && strings.Contains(a.Runner[0], "{{") {
			add("%s: runner[0] must not be templated", p)
		}
		if strings.Contains(a.Command, "{{") {
			add("%s: command must not be templated", p)
		}
		if a.Kind == "model" {
			if a.Model == "" {
				add("%s: model is required for kind model", p)
			}
			for _, m := range a.MCP {
				if strings.Contains(m, "__") { // tool names are mcp__<server>__<tool>
					add("%s: mcp source %q: names used by a model agent must not contain __", p, m)
				}
			}
			if cr := c.Credentials[a.Credential]; a.Credential != "" && cr != nil && !slices.Contains(modelProviders, cr.Provider) {
				add("%s: kind model needs an ollama or openai credential, %q is %s", p, a.Credential, cr.Provider)
			}
		} else if !slices.Contains(kinds, a.Kind) {
			add("%s: kind must be claude, codex, agy or model, got %q", p, a.Kind)
		} else if len(a.Runner) > 0 && a.Kind != "claude" {
			add("%s: runner implies kind claude, got %s", p, a.Kind)
		}
		if a.Credential == "" {
			// Pre-credentials configs (claude, none declared) keep their ambient login until step 5.
			if a.Kind != "claude" || c.declaredCredentials() > 0 {
				add("%s: credential is required (no unique %s subscription credential to default to)", p, a.Kind)
			}
		} else if cr := c.Credentials[a.Credential]; cr == nil {
			add("%s: unknown credential %q", p, a.Credential)
		} else if a.Kind != "model" && cr.Provider != a.Kind {
			add("%s: credential %q is for %s, agent kind is %s", p, a.Credential, cr.Provider, a.Kind)
		}
		if a.APIKeyFile != "" && !safePath.MatchString(a.APIKeyFile) {
			add("%s: api_key_file must be an absolute path of [A-Za-z0-9/._-]", p)
		}
		checkAllow(p, a.Egress.Allow)
		checkTemplate(p+" prompt", a.Prompt, add)
	}

	for _, name := range sortedKeys(c.Routines) {
		rt := c.Routines[name]
		p := "routines." + name
		if rt == nil || len(rt.Steps) == 0 {
			add("%s: needs at least one step", p)
			continue
		}
		ids := map[string]bool{}
		for i, st := range rt.Steps {
			sp := fmt.Sprintf("%s.steps[%d]", p, i)
			if st.ID == "" {
				add("%s: id is required", sp)
			} else if ids[st.ID] {
				add("%s: duplicate step id %q", sp, st.ID)
			}
			ids[st.ID] = true
			if st.Timeout < 0 {
				add("%s: timeout must not be negative", sp)
			}
			if st.Agent != "" && st.Retry != nil {
				add("%s: retry is not allowed on agent steps (paid runs would bypass the daily cap)", sp)
			}
			if r := st.Retry; r != nil {
				if r.Attempts < 0 || r.Attempts > MaxRetryAttempts {
					add("%s: retry.attempts must be 0..%d", sp, MaxRetryAttempts)
				}
				if r.Base < 0 || r.Factor < 0 || (r.Factor > 0 && r.Factor < 1) {
					add("%s: retry.base must be >= 0 and retry.factor >= 1", sp)
				}
			}
			checkExpr(sp+" if", st.If, add)
			c.validateAction(sp, Action{Cmd: st.Cmd, Unit: st.Unit, Agent: st.Agent}, add)
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validateSource(name string, s *Source, add func(string, ...any)) {
	p := "sources." + name
	if s == nil {
		add("%s: empty", p)
		return
	}
	if s.Poll < 0 || (s.Type != "webhook" && s.Poll == 0) {
		add("%s: poll must be > 0", p)
	}
	if s.Type == "mcp" || s.Type == "http" {
		if u, err := url.Parse(s.URL); err == nil && u.User != nil {
			add("%s: url must not contain credentials (user:pass@); use auth.bearer or headers", p)
		}
	}
	switch s.Type {
	case "mcp":
		if (s.URL == "") == (len(s.Command) == 0) {
			add("%s: set exactly one of url or command", p)
		}
		if s.Read == nil || (s.Read.Resource == "") == (s.Read.Tool == "") {
			add("%s: read needs exactly one of resource or tool", p)
		}
	case "http":
		if s.URL == "" {
			add("%s: url is required", p)
		}
		if s.Method != "" && s.Method != "GET" && s.Method != "POST" {
			add("%s: method must be GET or POST", p)
		}
	case "webhook":
		if !s.Secret.isSet() {
			add("%s: secret is required", p)
		}
		if s.ID != "" && !webhookID.MatchString(s.ID) {
			add("%s: id must look like header.<Name>, got %q", p, s.ID)
		}
		if s.Signature == "github" && s.TimestampHdr != "" {
			add("%s: timestamp_header is not allowed with signature github (GitHub does not sign timestamps)", p)
		}
		switch s.Signature {
		case "github":
		case "sha256":
			if s.SigHeader == "" {
				add("%s: signature sha256 needs signature_header", p)
			} else if strings.EqualFold(s.SigHeader, s.TimestampHdr) {
				add("%s: signature_header and timestamp_header must differ", p)
			}
		case "token":
			if s.TokenHeader == "" {
				add("%s: signature token needs token_header", p)
			}
		case "standard-webhooks":
			if s.SigHeader != "" || s.TimestampHdr != "" {
				add("%s: signature_header and timestamp_header are not allowed with signature standard-webhooks", p)
			}
		default:
			add("%s: signature must be github, sha256, token or standard-webhooks, got %q", p, s.Signature)
		}
	default:
		add("%s: type must be mcp, http or webhook, got %q", p, s.Type)
	}
	if len(s.Env) > 0 && (s.Type != "mcp" || len(s.Command) == 0) {
		add("%s: env needs a stdio MCP source (type mcp with command)", p)
	}
	for _, k := range sortedKeys(s.Env) {
		if !envName.MatchString(k) {
			add("%s: env name %q must match ^[A-Z_][A-Z0-9_]*$", p, k)
		} else if envDenied.MatchString(k) {
			add("%s: env name %q is not allowed (it changes how the child loads code or connects)", p, k)
		}
	}
	if name == AgentResultSource {
		add("%s: name is reserved", p)
	}
}

func (c *Config) validateAction(p string, a Action, add func(string, ...any)) {
	n := 0
	if len(a.Cmd) > 0 {
		n++
		if strings.Contains(a.Cmd[0], "{{") {
			add("%s: cmd[0] must not be templated", p)
		}
		for _, e := range a.Cmd {
			checkTemplate(p+" cmd", e, add)
		}
	}
	if a.Unit != "" {
		n++
		if strings.Contains(a.Unit, "{{") {
			add("%s: unit must not be templated", p)
		} else if !slices.Contains(c.Units, a.Unit) {
			add("%s: unit %q is not in the units allowlist", p, a.Unit)
		}
	}
	if a.Agent != "" {
		n++
		if _, ok := c.Agents[a.Agent]; !ok {
			add("%s: unknown agent %q", p, a.Agent)
		}
	}
	if a.Routine != "" {
		n++
		if _, ok := c.Routines[a.Routine]; !ok {
			add("%s: unknown routine %q", p, a.Routine)
		}
	}
	if n != 1 {
		add("%s: action needs exactly one of cmd, unit, agent, routine", p)
	}
}

// RoutineHasAgent reports whether the named routine has an agent step.
func (c *Config) RoutineHasAgent(name string) bool {
	if rt := c.Routines[name]; rt != nil {
		for _, s := range rt.Steps {
			if s.Agent != "" {
				return true
			}
		}
	}
	return false
}

func checkExpr(where, src string, add func(string, ...any)) {
	if src == "" {
		return
	}
	if _, err := expr.Compile(src, expr.Env(map[string]any{}), expr.AllowUndefinedVariables()); err != nil {
		add("%s: bad expression %q: %v", where, src, err)
	}
}

func checkTemplate(where, src string, add func(string, ...any)) {
	if _, err := template.New("").Parse(src); err != nil {
		add("%s: bad template %q: %v", where, src, err)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// implicitFor reports whether name is the api_key_file credential of an existing agent.
func implicitFor(c *Config, name string) bool {
	a := c.Agents[strings.TrimPrefix(name, implicitPrefix)]
	return a != nil && a.Credential == name && a.APIKeyFile != ""
}

// HostPort is one egress allowlist entry. Host is an exact name or *.suffix
// (subdomains only). The integrator maps it to the proxy's entry type.
type HostPort struct {
	Host         string
	Port         int
	AllowPrivate bool // from an allow_private MCP source or a listed private model endpoint
	NoLinkLocal  bool // with AllowPrivate: still refuse link-local and metadata addresses
}

func (h HostPort) String() string {
	s := fmt.Sprintf("%s:%d", h.Host, h.Port)
	if h.AllowPrivate {
		s += " (allow_private)"
	}
	if h.NoLinkLocal {
		s += " (no_link_local)"
	}
	return s
}

var hostName = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// parseHostPort reads host, host:port, *.suffix or *.suffix:port; the port defaults to 443.
func parseHostPort(e string) (HostPort, error) {
	host, port := e, 443
	if i := strings.LastIndex(e, ":"); i >= 0 {
		n, err := strconv.Atoi(e[i+1:])
		if err != nil || n < 1 || n > 65535 {
			return HostPort{}, fmt.Errorf("%q: port must be 1-65535", e)
		}
		host, port = e[:i], n
	}
	if !hostName.MatchString(host) {
		return HostPort{}, fmt.Errorf("%q: want host, host:port or *.suffix", e)
	}
	return HostPort{Host: strings.ToLower(host), Port: port}, nil
}

// providerHosts are the verified hosts each CLI needs (2026-10-06).
var providerHosts = map[string][2][]string{ // kind -> {subscription, api key}
	"claude": {{"api.anthropic.com", "platform.claude.com"}, {"api.anthropic.com"}},
	"codex":  {{"chatgpt.com", "auth.openai.com"}, {"api.openai.com"}},
	"agy": {{"oauth2.googleapis.com", "daily-cloudcode-pa.googleapis.com", "cloudcode-pa.googleapis.com", "www.googleapis.com", "lh3.googleusercontent.com"},
		{"generativelanguage.googleapis.com"}},
}

func (c *Config) userAllow(lists ...[]string) (out []HostPort) {
	for _, l := range lists {
		for _, e := range l {
			if hp, err := parseHostPort(e); err == nil {
				out = append(out, hp)
			}
		}
	}
	return out
}

func dedupe(l []HostPort) []HostPort {
	seen := map[HostPort]bool{}
	out := l[:0]
	for _, h := range l {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// AgentEgress returns the effective egress allowlist of the agent definition
// that actually runs (a job's snapshot, not the live config) and whether
// restriction is enabled. It fails closed: a nil agent or one without an
// egress setting (an old snapshot) is restricted.
func (c *Config) AgentEgress(a *Agent) (allow []HostPort, enabled bool) {
	if a == nil {
		return nil, true
	}
	if a.Egress.Enabled != nil && !*a.Egress.Enabled {
		return nil, false
	}
	mode := 0 // subscription (also for legacy agents with no credential)
	if cr := c.Credentials[a.Credential]; a.Credential != "" && cr != nil && cr.APIKey.isSet() {
		mode = 1
	}
	for _, h := range providerHosts[a.Kind][mode] {
		allow = append(allow, HostPort{Host: h, Port: 443})
	}
	if cr := c.Credentials[a.Credential]; a.Kind == "model" && cr != nil {
		if host, port, err := ModelURL(cr.URL); err == nil {
			listed := c.PrivateEndpoint(host, port)
			allow = append(allow, HostPort{Host: host, Port: port, AllowPrivate: listed, NoLinkLocal: listed})
		}
	}
	for _, m := range a.MCP {
		src := c.Sources[m]
		if src == nil || src.URL == "" {
			continue
		}
		u, err := url.Parse(src.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := 443
		if u.Scheme == "http" {
			port = 80
		}
		if p, err := strconv.Atoi(u.Port()); err == nil {
			port = p
		}
		allow = append(allow, HostPort{Host: strings.ToLower(u.Hostname()), Port: port, AllowPrivate: src.AllowPrivate})
	}
	allow = append(allow, c.userAllow(a.Egress.Allow, c.Server.Egress.Allow)...)
	return dedupe(allow), true
}

// RuleEgress returns the allowlist for a rule's cmd actions and whether they are restricted.
func (c *Config) RuleEgress(r Rule) (allow []HostPort, enabled bool) {
	if !r.Egress.Enabled && !c.Server.Egress.CmdDefault {
		return nil, false
	}
	return dedupe(c.userAllow(r.Egress.Allow, c.Server.Egress.Allow)), true
}

// IsModel reports whether the credential points at a model endpoint.
func (c *Credential) IsModel() bool { return slices.Contains(modelProviders, c.Provider) }

// BaseURL is the OpenAI-compatible base: the URL, plus /v1 for Ollama.
func (c *Credential) BaseURL() string {
	u := strings.TrimRight(c.URL, "/")
	if c.Provider == "ollama" {
		u += "/v1"
	}
	return u
}

// ModelURL checks a model endpoint URL (http(s)://host[:port][/path], no
// userinfo) and returns its lowercase host and port (default 80/443).
func ModelURL(raw string) (host string, port int, err error) {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", 0, errors.New("must be http(s)://host[:port][/path]")
	}
	if strings.ContainsAny(raw, "?#") { // would smuggle a query or fragment onto the paths we append
		return "", 0, errors.New("must not contain ? or #")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." || seg == "." {
			return "", 0, errors.New("must not contain . or .. path segments")
		}
	}
	port = 443
	if u.Scheme == "http" {
		port = 80
	}
	if u.Port() != "" {
		if port, e = strconv.Atoi(u.Port()); e != nil || port < 1 || port > 65535 {
			return "", 0, errors.New("port must be 1-65535")
		}
	}
	return strings.ToLower(u.Hostname()), port, nil
}

// endpointKey parses a host:port entry (explicit port, no wildcard).
func endpointKey(e string) (string, int, bool) {
	h, p, err := net.SplitHostPort(e)
	n, perr := strconv.Atoi(p)
	if err != nil || perr != nil || h == "" || n < 1 || n > 65535 || strings.Contains(h, "*") {
		return "", 0, false
	}
	return strings.ToLower(h), n, true
}

// PrivateEndpoint reports whether host:port is in server.models.private_endpoints.
func (c *Config) PrivateEndpoint(host string, port int) bool {
	for _, e := range c.Server.Models.PrivateEndpoints {
		if h, p, ok := endpointKey(e); ok && h == strings.ToLower(host) && p == port {
			return true
		}
	}
	return false
}
