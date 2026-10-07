// Package docs embeds the documentation: the Markdown pages and the task
// templates, for the CLI (`template`, `guide`) and the portal.
package docs

//go:generate sh -c "go run ../cmd/siphon help --json | go run ./internal/gencli > cli.md"

import (
	"embed"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// To embed a new directory of pages (tasks, connections, ...) once it has
// files, add its name to this directive.
//
//go:embed *.md templates tasks connections developing
var files embed.FS

// FS is every embedded file, rooted at docs/.
func FS() fs.FS { return files }

// Page is the Markdown page called name: "llm", "concepts", "tasks/webhook-command".
func Page(name string) ([]byte, bool) {
	for _, n := range []string{name, name + ".md"} {
		if b, err := files.ReadFile(n); err == nil && strings.HasSuffix(n, ".md") {
			return b, true
		}
	}
	return nil, false
}

// Template is one ready-made apply file and what its header says about it.
type Template struct {
	Name     string   `json:"name"`
	Title    string   `json:"title"`
	Category string   `json:"category"`
	Needs    []string `json:"needs"`   // items to connect first, like credentials/claude-max
	Secrets  []string `json:"secrets"` // secrets to pass, like sources/hook.secret
	Apply    string   `json:"apply"`
	Notes    string   `json:"notes"` // the sender setup notes under the header
	YAML     string   `json:"yaml,omitempty"`
}

// Categories is the display order; unknown ones sort after these.
var Categories = []string{"Code review & CI", "Ops & monitoring", "Notifications & glue", "Local LLM", "AWS", "MCP", "Homelab"}

var (
	header = regexp.MustCompile(`^\s*(title|category|needs|secrets|apply):\s*(.*)$`)
	parens = regexp.MustCompile(`\([^)]*\)`)
)

// Templates are sorted by category, then name.
func Templates() []Template {
	entries, _ := fs.ReadDir(files, "templates")
	var out []Template
	for _, e := range entries {
		n, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok {
			continue
		}
		b, _ := files.ReadFile("templates/" + e.Name())
		out = append(out, parse(n, string(b)))
	}
	rank := func(c string) int {
		for i, k := range Categories {
			if k == c {
				return i
			}
		}
		return len(Categories)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i].Category), rank(out[j].Category); ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Lookup finds a template by name.
func Lookup(name string) (Template, bool) {
	for _, t := range Templates() {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// parse reads the leading comment block: key lines, continuation lines under
// needs and secrets, then free notes.
func parse(name, src string) Template {
	t := Template{Name: name, YAML: src, Needs: []string{}, Secrets: []string{}}
	var notes []string
	raw := map[string]string{}
	last := ""
	for _, line := range strings.Split(src, "\n") {
		body, ok := strings.CutPrefix(line, "#")
		if !ok {
			break
		}
		if m := header.FindStringSubmatch(body); m != nil && len(notes) == 0 {
			raw[m[1]] = m[2]
			last = ""
			if m[1] == "needs" || m[1] == "secrets" {
				last = m[1]
			}
			continue
		}
		if last != "" && strings.HasPrefix(body, "  ") {
			raw[last] += " " + strings.TrimSpace(body)
			continue
		}
		last = ""
		notes = append(notes, strings.TrimPrefix(body, " "))
	}
	t.Title, t.Category, t.Apply = strings.TrimSpace(raw["title"]), strings.TrimSpace(raw["category"]), strings.TrimSpace(raw["apply"])
	t.Needs, t.Secrets = refs(raw["needs"]), refs(raw["secrets"])
	t.Notes = strings.TrimRight(strings.Join(notes, "\n"), "\n ")
	return t
}

// refs pulls the kind/name tokens out of a header value, dropping the
// parenthetical hints.
func refs(v string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(parens.ReplaceAllString(v, " "), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if strings.Contains(f, "/") {
			out = append(out, f)
		}
	}
	return out
}
