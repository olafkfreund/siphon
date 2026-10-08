package catalog

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/olafkfreund/siphon/internal/config"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// UserError is a refusal to show the user (bad input, an unmet precondition).
type UserError struct{ Msg, Field string } // Field names the form field at fault, if one is

func (e UserError) Error() string { return e.Msg }

func bad(f string, a ...any) error { return UserError{Msg: fmt.Sprintf(f, a...)} }

func badf(field, f string, a ...any) error { return UserError{fmt.Sprintf(f, a...), field} }

// Env is what Render needs from the daemon.
type Env struct {
	Config     *config.Config
	SecretPath func(kind, name, key string) string // where a write-only secret will live
	HookURL    func(name string) string
}

type Item struct{ Kind, Name, YAML string }

// Secret is a write-only value, stored as a file at SecretPath(Kind, Name, Key).
type Secret struct{ Kind, Name, Key, Value string }

// Done is shown once after setup: the webhook secret never again.
type Done struct {
	Service    string `json:"service"`
	Name       string `json:"name"`
	Hook       string `json:"hook,omitempty"`
	HookURL    string `json:"hook_url,omitempty"`
	HookSecret string `json:"hook_secret,omitempty"`
	HookHeader string `json:"hook_header,omitempty"`
	ReadTools  string `json:"read_tools,omitempty"`
	WriteTools string `json:"write_tools,omitempty"`
	Setup      string `json:"setup,omitempty"`
	ApplyErr   string `json:"apply_error,omitempty"` // the save worked but applying it live failed
}

type Result struct {
	Items   []Item
	Secrets []Secret
	Done    *Done
}

// Ctx is what a hook sees and may change.
type Ctx struct {
	Entry  *Entry
	Values map[string]string
	Cfg    *config.Config
	Data   map[string]any // extra template data
	Done   *Done
}

var hooks = map[string]func(*Ctx) error{"aws": awsHook}

// Funcs that need no state; the real secret funcs are bound per item in Render.
var stubFuncs = template.FuncMap{
	"q": quote, "pathesc": pathEsc, "package": func(string) bool { return false }, "private": func(string) bool { return false },
	"has": func(string) bool { return false }, "fail": fail, "tools": tools,
	"secret": func(string, string, ...string) string { return "" }, "basic": func(string, string, string) string { return "" },
	"generated": func(string, ...string) string { return "" },
}

func quote(s string) string           { return strconv.Quote(s) }
func pathEsc(s string) string         { return url.PathEscape(strings.Trim(s, "/")) }
func fail(msg string) (string, error) { return "", UserError{Msg: msg} }
func tools(src string, names ...string) string {
	out := make([]string, len(names))
	for i, t := range names {
		out[i] = "mcp__" + src + "__" + t
	}
	return "[" + strings.Join(out, ", ") + "]"
}

func loopback(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return h == "localhost"
}

// NewSecret is a random webhook secret.
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

var itemName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Render checks the values and builds the items. Field values reach the YAML
// only as validated one-line text (and quoted with q); secrets only through
// the secret, basic and generated functions. Nothing is stored here.
func (e *Entry) Render(values map[string]string, env Env) (*Result, error) {
	if st, why := e.Availability(env.Config); st != "available" {
		return nil, bad("%s can't be connected here: %s", e.Name, why)
	}
	vals, err := e.clean(values, env.Config)
	if err != nil {
		return nil, err
	}
	data := map[string]any{}
	for _, f := range e.Fields {
		switch f.Type {
		case "secret":
		case "bool":
			data[f.Key] = vals[f.Key] != ""
		case "multi":
			data[f.Key] = splitList(vals[f.Key])
		default:
			data[f.Key] = vals[f.Key]
		}
	}
	res := &Result{Done: &Done{Service: e.Name}}
	ctx := &Ctx{Entry: e, Values: vals, Cfg: env.Config, Data: data, Done: res.Done}
	if h := hooks[e.Hook]; h != nil {
		if err := h(ctx); err != nil {
			return nil, err
		}
	}
	name := vals["name"]
	res.Done.Name = name
	if len(e.ReadTools) > 0 {
		res.Done.ReadTools = tools(name, e.ReadTools...)
		res.Done.WriteTools = tools(name, append(slices.Clone(e.ReadTools), e.WriteTools...)...)
	}
	for i, c := range e.Creates {
		on, err := e.exec(fmt.Sprintf("%d/when", i), data, nil)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(on) != "true" {
			continue
		}
		iname, err := e.exec(fmt.Sprintf("%d/name", i), data, nil)
		if err != nil {
			return nil, err
		}
		if !itemName.MatchString(iname) {
			return nil, bad("%q is not a valid item name", iname)
		}
		fm := e.itemFuncs(c, iname, vals, env, res)
		y, err := e.exec(fmt.Sprintf("%d/yaml", i), data, fm)
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(y, "\n") {
			y += "\n"
		}
		if c.Hook && res.Done.Hook == "" {
			res.Done.Hook = iname
			if env.HookURL != nil {
				res.Done.HookURL = env.HookURL(iname)
			}
		}
		y += "connection: " + quote(name) + "\nservice: " + e.ID + "\n"
		res.Items = append(res.Items, Item{c.Kind, iname, y})
	}
	if env.Config != nil {
		for _, src := range env.Config.Sources {
			if src.Connection == name {
				return nil, badf("name", "a connection named %s already exists (service %s)", name, src.Service)
			}
		}
		for _, cr := range env.Config.Credentials {
			if cr.Connection == name {
				return nil, badf("name", "a connection named %s already exists (service %s)", name, cr.Service)
			}
		}
		for _, it := range res.Items {
			if it.Kind == "sources" && env.Config.Sources[it.Name] != nil {
				return nil, badf("name", "a source named %s already exists; pick another name or edit it", it.Name)
			}
			if it.Kind == "credentials" && env.Config.Credentials[it.Name] != nil {
				return nil, badf("name", "a credential named %s already exists; pick another name", it.Name)
			}
		}
	}
	if e.Setup != "" {
		if res.Done.Setup, err = e.exec("setup", data, nil); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// exec runs a parsed template; fm (if any) replaces the stub funcs.
func (e *Entry) exec(key string, data map[string]any, fm template.FuncMap) (string, error) {
	t := e.tmpl[key]
	if fm != nil {
		var err error
		if t, err = t.Clone(); err != nil {
			return "", err
		}
		t = t.Funcs(fm)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		var ue UserError
		if errors.As(err, &ue) {
			return "", ue
		}
		return "", fmt.Errorf("catalog %s: %w", e.ID, err)
	}
	return b.String(), nil
}

func (e *Entry) itemFuncs(c Create, iname string, vals map[string]string, env Env, res *Result) template.FuncMap {
	path := func(key string) string {
		if env.SecretPath != nil {
			return env.SecretPath(c.Kind, iname, key)
		}
		return "/secrets/" + c.Kind + "--" + iname + "+" + key
	}
	store := func(key, value string) string {
		res.Secrets = append(res.Secrets, Secret{c.Kind, iname, key, value})
		return "file:" + path(key)
	}
	return template.FuncMap{
		"package": func(n string) bool {
			if env.Config == nil {
				return true
			}
			_, ok := env.Config.Server.MCPPackages[n]
			return ok
		},
		"private": func(field string) bool { return env.Config != nil && env.Config.ServiceEndpoint(vals[field]) },
		"has":     func(field string) bool { return vals[field] != "" },
		// secret "field" "yaml.path" ["prefix"]: the field's value as a
		// write-only file ref; a prefix such as "Bearer " is stored with it.
		"secret": func(field, key string, prefix ...string) (string, error) {
			if e.field(field) == nil {
				return "", fmt.Errorf("secret: no field %q", field)
			}
			return store(key, strings.Join(prefix, "")+vals[field]), nil
		},
		// basic "emailField" "tokenField" "yaml.path": Basic base64(email:token), as a secret.
		"basic": func(email, token, key string) (string, error) {
			if f := e.field(token); f == nil || f.Type != "secret" {
				return "", fmt.Errorf("basic: %q is not a secret field", token)
			}
			return store(key, "Basic "+base64.StdEncoding.EncodeToString([]byte(vals[email]+":"+vals[token]))), nil
		},
		// generated "yaml.path" ["prefix"]: a random webhook secret, shown once
		// on the done page (the prefix, such as "Bearer ", is stored but not shown).
		"generated": func(key string, prefix ...string) string {
			v := NewSecret()
			res.Done.Hook, res.Done.HookSecret, res.Done.HookHeader = iname, v, c.HookHeader
			if env.HookURL != nil {
				res.Done.HookURL = env.HookURL(iname)
			}
			return store(key, strings.Join(prefix, "")+v)
		},
	}
}

func (e *Entry) field(key string) *Field {
	for i := range e.Fields {
		if e.Fields[i].Key == key {
			return &e.Fields[i]
		}
	}
	return nil
}

// clean validates each field and returns the values to use.
func (e *Entry) clean(in map[string]string, cfg *config.Config) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range e.Fields {
		v := in[f.Key]
		v = strings.TrimSpace(v)
		if f.Type == "bool" { // unchecked means off: a default is only the form's initial state
			if v != "" {
				out[f.Key] = "on"
			}
			continue
		}
		if strings.TrimSpace(v) == "" && f.Type != "multi" {
			v = f.Default
		}
		if strings.TrimSpace(v) == "" {
			if f.Required {
				return nil, badf(f.Key, "%s is required", f.Label)
			}
			continue
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, badf(f.Key, "%s must be a single line", f.Label)
		}
		switch f.Type {
		case "choice":
			if !slices.Contains(f.Choices, v) {
				return nil, badf(f.Key, "%s: choose one of %s", f.Label, strings.Join(f.Choices, ", "))
			}
		case "multi":
			for _, p := range splitList(v) {
				if !slices.Contains(f.Choices, p) {
					return nil, badf(f.Key, "%s: %q is not one of %s", f.Label, p, strings.Join(f.Choices, ", "))
				}
			}
		case "url":
			v = strings.TrimRight(v, "/")
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, badf(f.Key, "%s must be http(s)://host[:port][/path]", f.Label)
			}
			if f.PrivateListed && config.PrivateHost(u.Hostname()) && (cfg == nil || !cfg.ServiceEndpoint(v)) {
				return nil, badf(f.Key, "%s is a private address: list %s in server.services.private_endpoints first", f.Label, u.Host)
			}
			if f.Secure && u.Scheme != "https" && !loopback(u.Hostname()) && (cfg == nil || !cfg.ServiceEndpoint(v)) {
				return nil, badf(f.Key, "the %s must be https (http only for localhost or a host listed in server.services.private_endpoints): the token would travel in clear", f.Label)
			}
		}
		if f.re != nil && !f.re.MatchString(v) {
			msg := f.PatternE
			if msg == "" {
				msg = f.Label + " has an invalid format"
			}
			return nil, badf(f.Key, "%s", msg)
		}
		out[f.Key] = v
	}
	return out, nil
}
