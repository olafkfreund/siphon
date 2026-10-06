// Package config loads and validates agentgw.yaml, the only source of truth.
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
	Server   Server              `yaml:"server"`
	Limits   Limits              `yaml:"limits"`
	Sources  map[string]*Source  `yaml:"sources"`
	Rules    []Rule              `yaml:"rules"`
	Agents   map[string]*Agent   `yaml:"agents"`
	Routines map[string]*Routine `yaml:"routines"`
	Units    []string            `yaml:"units"`

	resolveErrs []error
}

type Server struct {
	Listen  string `yaml:"listen"`
	DB      string `yaml:"db"`
	Workers int    `yaml:"workers"`
	Token   Secret `yaml:"token"`
	Sandbox string `yaml:"sandbox"` // systemd|none
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
	Signature    string            `yaml:"signature"`        // github|sha256
	SigHeader    string            `yaml:"signature_header"` // sha256 preset
	TimestampHdr string            `yaml:"timestamp_header"`
	ID           string            `yaml:"id"` // delivery id, e.g. header.X-GitHub-Delivery
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
	Name             string   `yaml:"name"`
	Source           string   `yaml:"source"`
	ForEach          string   `yaml:"for_each"`
	ID               string   `yaml:"id"`
	When             string   `yaml:"when"`
	On               string   `yaml:"on"` // edge (default)|each
	Repeat           Duration `yaml:"repeat"`
	Cooldown         Duration `yaml:"cooldown"`
	AllowAgentEvents bool     `yaml:"allow_agent_events"`
	Action           Action   `yaml:"action"`
	Approve          bool     `yaml:"approve"`
}

type Action struct {
	Cmd     []string `yaml:"cmd"`
	Unit    string   `yaml:"unit"`
	Agent   string   `yaml:"agent"`
	Routine string   `yaml:"routine"`
}

type Agent struct {
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

type Retry struct {
	Attempts int `yaml:"attempts"`
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
	// A relative db lives next to the config file, wherever the daemon is started.
	if d := c.Server.DB; d != "" && d != ":memory:" && !filepath.IsAbs(d) {
		c.Server.DB = filepath.Join(filepath.Dir(path), d)
	}
	return c, nil
}

func Parse(b []byte) (*Config, error) {
	c := &Config{
		Server: Server{Listen: ":8080", DB: "agentgw.db", Workers: 4, Sandbox: "systemd"},
		Limits: Limits{AgentRunsPerDay: 50, HTTPMaxBody: 1 << 20, HTTPTimeout: Duration(30 * time.Second)},
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
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
		if a.Approve == nil {
			t := true
			a.Approve = &t
		}
		if a.Timeout == 0 {
			a.Timeout = Duration(10 * time.Minute)
		}
	}
	for _, s := range c.secretPtrs() {
		if err := s.resolve(); err != nil {
			c.resolveErrs = append(c.resolveErrs, err)
		}
	}
	for _, name := range sortedKeys(c.Sources) {
		if src := c.Sources[name]; src != nil {
			for _, h := range sortedKeys(src.Headers) {
				v := src.Headers[h]
				if err := v.resolve(); err != nil {
					c.resolveErrs = append(c.resolveErrs, err)
				}
				src.Headers[h] = v
			}
		}
	}
	return c, nil
}

// secretPtrs are values that must always be env:/file: refs. Source headers
// are handled separately: they may be plain literals unless the name looks secret.
func (c *Config) secretPtrs() []*Secret {
	out := []*Secret{&c.Server.Token}
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

func (s *Secret) resolve() error {
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
		if err != nil {
			return fmt.Errorf("secret %s: %w", s.Ref, err)
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
		}
	}
	return out
}

// Warnings lists non-fatal findings.
func (c *Config) Warnings() []string {
	var w []string
	if c.Server.Sandbox == "none" {
		w = append(w, "server.sandbox is none: actions run unsandboxed")
	}
	for _, name := range sortedKeys(c.Sources) {
		if s := c.Sources[name]; s != nil && s.AllowPrivate {
			w = append(w, fmt.Sprintf("source %s: allow_private disables the private-address guard", name))
		}
		if s := c.Sources[name]; s != nil && s.Auth != nil && s.Auth.Bearer.isSet() && cleartextRemote(s.URL) {
			w = append(w, fmt.Sprintf("source %s: bearer token sent over plain http to a non-loopback host", name))
		}
	}
	return w
}

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

	if c.Limits.HTTPTimeout < 0 {
		add("limits.http_timeout: must not be negative")
	}

	for _, u := range c.Units {
		if strings.Contains(u, "{{") {
			add("units: %q must not be templated", u)
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
		c.validateAction(p, r.Action, add)
		if (r.Action.Agent != "" || (r.Action.Routine != "" && c.routineHasAgent(r.Action.Routine))) && r.Cooldown <= 0 {
			add("%s: cooldown is mandatory for agent actions", p)
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
		if a.APIKeyFile != "" && !safePath.MatchString(a.APIKeyFile) {
			add("%s: api_key_file must be an absolute path of [A-Za-z0-9/._-]", p)
		}
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
		switch s.Signature {
		case "github":
		case "sha256":
			if s.SigHeader == "" {
				add("%s: signature sha256 needs signature_header", p)
			}
		default:
			add("%s: signature must be github or sha256, got %q", p, s.Signature)
		}
	default:
		add("%s: type must be mcp, http or webhook, got %q", p, s.Type)
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

func (c *Config) routineHasAgent(name string) bool {
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
