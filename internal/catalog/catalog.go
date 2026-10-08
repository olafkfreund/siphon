// Package catalog is the single source of truth for the services Siphon can
// connect (services.yaml, embedded). An entry is data: fields, and a list of
// config items to create from text/template YAML. Render turns the values
// into items, write-only secrets and the one-time done-info; the portal and
// API commit them as one config revision.
package catalog

import (
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/internal/config"
)

//go:embed services.yaml
var servicesYAML []byte

type Entry struct {
	ID           string   `yaml:"id" json:"id"`
	Name         string   `yaml:"name" json:"name"`
	Mark         string   `yaml:"mark" json:"mark"`
	Category     string   `yaml:"category" json:"category"`
	Summary      string   `yaml:"summary" json:"summary"`
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	Status       string   `yaml:"status" json:"status"`
	Reason       string   `yaml:"reason" json:"reason,omitempty"`
	Fields       []Field  `yaml:"fields" json:"fields"`
	Creates      []Create `yaml:"creates" json:"-"`
	Setup        string   `yaml:"setup" json:"-"` // template: provider-side steps
	ReadTools    []string `yaml:"read_tools" json:"-"`
	WriteTools   []string `yaml:"write_tools" json:"-"`
	Test         *Test    `yaml:"test" json:"-"`
	Templates    []string `yaml:"templates" json:"templates,omitempty"`
	Hook         string   `yaml:"hooks" json:"-"`                               // a registered Go hook (aws only)
	NeedsPackage []string `yaml:"needs_package" json:"needs_package,omitempty"` // connectable once any one of these is in server.mcp_packages

	tmpl map[string]*template.Template // by "create/<i>/name|when|yaml", "setup"
}

type Field struct {
	Key      string   `yaml:"key" json:"key"`
	Label    string   `yaml:"label" json:"label"`
	Type     string   `yaml:"type" json:"type"` // text|secret|url|choice|multi|bool
	Default  string   `yaml:"default" json:"default,omitempty"`
	Help     string   `yaml:"help" json:"help,omitempty"`
	Required bool     `yaml:"required" json:"required,omitempty"`
	Choices  []string `yaml:"choices" json:"choices,omitempty"`
	Pattern  string   `yaml:"pattern" json:"pattern,omitempty"` // text: the whole value must match
	PatternE string   `yaml:"pattern_error" json:"-"`           // the message when it doesn't
	Secure   bool     `yaml:"secure" json:"-"`                  // url: https unless loopback or a listed private endpoint
	Section  int      `yaml:"section" json:"-"`                 // connect page section 1-3; default: bool 3, secret 2, else 1

	re *regexp.Regexp
}

// Create is one config item. Name, When and YAML are templates.
type Create struct {
	Kind       string `yaml:"kind"`
	Name       string `yaml:"name"`
	When       string `yaml:"when"`
	YAML       string `yaml:"yaml"`
	HookHeader string `yaml:"hook_header"` // a generated secret here is the webhook secret, sent in this header
}

// Test is how a connection is checked: a request and where the identity is.
type Test struct {
	Method   string `yaml:"method"`
	URL      string `yaml:"url"`
	Header   string `yaml:"header"`
	Body     string `yaml:"body"`
	Identity string `yaml:"identity"` // JSON path of the account name
}

var (
	categories = []string{"code", "issues", "chat", "monitoring", "cloud", "payments", "home", "generic"}
	statuses   = []string{"available", "needs-package", "not-yet"}
	fieldTypes = []string{"text", "secret", "url", "choice", "multi", "bool"}
	idRe       = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

var (
	once    sync.Once
	entries []*Entry
	loadErr error
)

// All is the embedded catalogue, in file order.
func All() []*Entry {
	once.Do(func() { entries, loadErr = Load(servicesYAML) })
	if loadErr != nil {
		panic("catalog: " + loadErr.Error()) // the embedded file is checked by a test
	}
	return entries
}

// Get returns the entry with that id, or nil.
func Get(id string) *Entry {
	for _, e := range All() {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// Load parses and checks a catalogue, templates included.
func Load(data []byte) ([]*Entry, error) {
	var doc struct {
		Services []*Entry `yaml:"services"`
	}
	dec := yaml.NewDecoder(bytesReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range doc.Services {
		if !idRe.MatchString(e.ID) || seen[e.ID] {
			return nil, fmt.Errorf("service %q: bad or duplicate id", e.ID)
		}
		seen[e.ID] = true
		if err := e.check(); err != nil {
			return nil, fmt.Errorf("service %s: %w", e.ID, err)
		}
	}
	return doc.Services, nil
}

func (e *Entry) check() error {
	if len(e.Mark) != 2 || e.Name == "" || e.Summary == "" {
		return fmt.Errorf("name, summary and a 2-letter mark are required")
	}
	if !slices.Contains(categories, e.Category) {
		return fmt.Errorf("unknown category %q", e.Category)
	}
	if !slices.Contains(statuses, e.Status) {
		return fmt.Errorf("unknown status %q", e.Status)
	}
	if (e.Status == "not-yet" || len(e.NeedsPackage) > 0) && e.Reason == "" {
		return fmt.Errorf("a not-yet service needs a reason")
	}
	if e.Hook != "" && hooks[e.Hook] == nil {
		return fmt.Errorf("unknown hook %q", e.Hook)
	}
	keys := map[string]bool{}
	for i := range e.Fields {
		f := &e.Fields[i]
		if f.Key == "" || keys[f.Key] || !slices.Contains(fieldTypes, f.Type) {
			return fmt.Errorf("field %q: bad key, duplicate or unknown type %q", f.Key, f.Type)
		}
		keys[f.Key] = true
		if f.Pattern != "" {
			re, err := regexp.Compile(`^(?:` + f.Pattern + `)$`)
			if err != nil {
				return fmt.Errorf("field %s: %w", f.Key, err)
			}
			f.re = re
		}
	}
	e.tmpl = map[string]*template.Template{}
	parse := func(key, text string) error {
		t, err := template.New(key).Funcs(stubFuncs).Option("missingkey=error").Parse(text)
		e.tmpl[key] = t
		return err
	}
	if err := parse("setup", e.Setup); err != nil {
		return err
	}
	for i, c := range e.Creates {
		if c.Kind != "sources" && c.Kind != "credentials" {
			return fmt.Errorf("create %d: kind must be sources or credentials", i)
		}
		for k, text := range map[string]string{"name": c.Name, "when": c.When, "yaml": c.YAML} {
			if err := parse(fmt.Sprintf("%d/%s", i, k), text); err != nil {
				return fmt.Errorf("create %d %s: %w", i, k, err)
			}
		}
	}
	return nil
}

// Availability is the entry's status on this install: not-yet stays, and an
// entry with needs_package is needs-package until one of them is installed.
// The reason says what to do.
func (e *Entry) Availability(cfg *config.Config) (status, reason string) {
	if e.Status != "available" && e.Status != "needs-package" {
		return e.Status, e.Reason
	}
	if len(e.NeedsPackage) > 0 && cfg != nil {
		for _, p := range e.NeedsPackage {
			if _, ok := cfg.Server.MCPPackages[p]; ok {
				return "available", ""
			}
		}
		return "needs-package", e.Reason
	}
	return "available", ""
}
