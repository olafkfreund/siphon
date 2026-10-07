package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// field is one form input and the dotted YAML path it maps to. Kinds:
// text, area, select, int, float, check (absent when off), tri ("", true,
// false), lines (one list item per line), csv, secret (write-only).
type field struct {
	Key, Label, Kind string
	Opts             []string
}

// kindFields is the explicit form <-> YAML mapping per config kind. Anything
// not listed (routine steps, source headers, rule egress, ...) is edited on
// the YAML tab and survives a form save untouched.
var kindFields = map[string][]field{
	"rules": {
		{"source", "Source", "text", nil}, {"when", "When (expression)", "text", nil},
		{"on", "On", "select", []string{"", "edge", "each"}}, {"id", "Event id", "text", nil},
		{"for_each", "For each", "text", nil}, {"repeat", "Repeat", "text", nil}, {"cooldown", "Cooldown", "text", nil},
		{"approve", "Needs approval", "check", nil},
		{"action.cmd", "Command (one argument per line)", "lines", nil}, {"action.agent", "Agent", "text", nil},
		{"action.unit", "Unit", "text", nil}, {"action.routine", "Routine", "text", nil},
	},
	"sources": {
		{"type", "Type", "select", []string{"http", "mcp", "webhook"}}, {"url", "URL", "text", nil},
		{"read.resource", "Read resource", "text", nil}, {"read.tool", "Read tool", "text", nil},
		{"poll", "Poll every", "text", nil},
		{"method", "Method", "select", []string{"", "GET", "POST"}}, {"body", "Body", "area", nil},
		{"secret", "Webhook secret", "secret", nil},
		{"signature", "Signature", "select", []string{"", "github", "sha256"}},
		{"signature_header", "Signature header", "text", nil}, {"timestamp_header", "Timestamp header", "text", nil},
		{"id", "Delivery id", "text", nil}, {"auth.bearer", "Bearer token", "secret", nil},
	},
	"agents": {
		{"kind", "Kind", "select", []string{"claude", "codex", "agy", "model"}}, {"credential", "Connection", "text", nil},
		{"model", "Model (kind: model)", "model", nil},
		{"command", "Command override", "text", nil}, {"prompt", "Prompt", "area", nil},
		{"mcp", "MCP sources (comma separated)", "csv", nil}, {"allowed_tools", "Allowed tools (comma separated)", "csv", nil},
		{"max_turns", "Max turns", "int", nil}, {"max_budget_usd", "Max budget (USD)", "float", nil},
		{"timeout", "Timeout", "text", nil}, {"approve", "Approval", "tri", nil},
		{"egress.enabled", "Egress restriction", "tri", nil}, {"egress.allow", "Egress allow (comma separated)", "csv", nil},
	},
	"credentials": {
		{"provider", "Provider", "select", []string{"claude", "codex", "agy"}},
		{"concurrency", "Concurrency", "int", nil}, {"api_key", "API key", "secret", nil},
	},
	"routines": nil,
}

// advanced fields are folded under "More options" so the essentials come first.
var advanced = map[string]bool{
	"agents/command": true, "agents/max_turns": true, "agents/max_budget_usd": true, "agents/timeout": true,
	"agents/egress.enabled": true, "credentials/concurrency": true,
}

// fieldView is a field with its current value, for the template.
type fieldView struct {
	field
	Adv     bool   // shown under "More options"
	Name    string // form input name
	Value   string
	Checked bool
	Set     bool // secret: a value or ref is stored
}

func decodeMap(y string) map[string]any {
	m := map[string]any{}
	_ = yaml.Unmarshal([]byte(y), &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func getPath(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(m map[string]any, path string, v any) { // v nil deletes, pruning empty parents
	parts := strings.Split(path, ".")
	var walk func(m map[string]any, i int)
	walk = func(m map[string]any, i int) {
		if i == len(parts)-1 {
			if v == nil {
				delete(m, parts[i])
			} else {
				m[parts[i]] = v
			}
			return
		}
		child, ok := m[parts[i]].(map[string]any)
		if !ok {
			if v == nil {
				return
			}
			child = map[string]any{}
			m[parts[i]] = child
		}
		walk(child, i+1)
		if len(child) == 0 {
			delete(m, parts[i])
		}
	}
	walk(m, 0)
}

func listStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, e := range l {
			out = append(out, fmt.Sprint(e))
		}
	}
	return out
}

// formFields shows the item YAML as form inputs. Secrets are never rendered back.
func formFields(kind, y string) []fieldView {
	m := decodeMap(y)
	var out []fieldView
	for _, f := range kindFields[kind] {
		fv := fieldView{field: f, Name: "f." + f.Key, Adv: advanced[kind+"/"+f.Key]}
		v, ok := getPath(m, f.Key)
		switch f.Kind {
		case "secret":
			fv.Set = ok && v != nil && fmt.Sprint(v) != ""
		case "check":
			fv.Checked, _ = v.(bool)
		case "lines":
			fv.Value = strings.Join(listStrings(v), "\n")
		case "csv":
			fv.Value = strings.Join(listStrings(v), ", ")
		default:
			if ok && v != nil {
				fv.Value = fmt.Sprint(v)
			}
		}
		out = append(out, fv)
	}
	return out
}

// secretsDir is where pasted secret values live, next to the database.
func secretsDir(db string) string { return filepath.Join(filepath.Dir(db), "secrets") }

// pendingSecret is a pasted secret value waiting for its save: the item YAML
// already holds its final file: ref, but the file is only written by commit,
// after the revision check and validation pass.
type pendingSecret struct{ Kind, Name, Key, Value string }

func (p pendingSecret) path(dir string) string {
	return filepath.Join(dir, p.Kind+"-"+p.Name+"-"+strings.ReplaceAll(p.Key, ".", "-"))
}

// writeSecret stores the value at p.path(dir), mode 0600, in a dir that is 0700
// and not a symlink: a temp file created exclusively (no following links),
// synced, then renamed over the target.
func writeSecret(dir string, p pendingSecret) error {
	if !itemName.MatchString(p.Name) { // the name is part of the file name: keep it inside dir
		return errors.New("bad item name")
	}
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return errors.New("secrets directory is not a plain directory")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	} else {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	final := p.path(dir)
	tmp := filepath.Join(dir, "."+filepath.Base(final)+"."+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(p.Value)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// applyForm layers the submitted form onto the existing item YAML and returns
// the new item YAML. A pasted secret value becomes a pending secret and only
// its final file: ref goes into the YAML; an empty secret field keeps what is there.
func applyForm(kind, name, existing string, form url.Values, db string) (string, []pendingSecret, error) {
	m := decodeMap(existing)
	var pending []pendingSecret
	for _, f := range kindFields[kind] {
		raw := strings.TrimSpace(form.Get("f." + f.Key))
		var v any
		switch f.Kind {
		case "check":
			if form.Get("f."+f.Key) != "" {
				v = true
			}
		case "tri":
			if raw != "" {
				v = raw == "true"
			}
		case "int":
			if raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil {
					return "", nil, fmt.Errorf("%s: not a whole number", f.Label)
				}
				v = n
			}
		case "float":
			if raw != "" {
				n, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					return "", nil, fmt.Errorf("%s: not a number", f.Label)
				}
				v = n
			}
		case "lines":
			var l []any
			for _, ln := range strings.Split(form.Get("f."+f.Key), "\n") {
				if ln = strings.TrimRight(ln, "\r"); strings.TrimSpace(ln) != "" {
					l = append(l, ln)
				}
			}
			if l != nil {
				v = l
			}
		case "csv":
			var l []any
			for _, e := range strings.Split(raw, ",") {
				if e = strings.TrimSpace(e); e != "" {
					l = append(l, e)
				}
			}
			if l != nil {
				v = l
			}
		case "secret":
			val := form.Get("f." + f.Key)
			switch {
			case val == "":
				continue // keep the stored ref
			case strings.HasPrefix(val, "env:") || strings.HasPrefix(val, "file:"):
				v = strings.TrimSpace(val)
			default:
				ps := pendingSecret{Kind: kind, Name: name, Key: f.Key, Value: val}
				pending = append(pending, ps)
				v = "file:" + ps.path(secretsDir(db))
			}
		case "area":
			if form.Get("f."+f.Key) != "" {
				v = strings.ReplaceAll(form.Get("f."+f.Key), "\r\n", "\n")
			}
		default:
			if raw != "" {
				v = raw
			}
		}
		setPath(m, f.Key, v)
	}
	if k := form.Get("action_kind"); kind == "rules" && k != "" { // the Then selector: only its field counts
		for _, o := range []string{"cmd", "agent", "unit", "routine"} {
			if o != k {
				setPath(m, "action."+o, nil)
			}
		}
	}
	b, err := yaml.Marshal(m)
	return string(b), pending, err
}
