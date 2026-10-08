package docs_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mdLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// Every relative link in the guide (and AGENTS.md, README.md) points at a
// file that exists, so the portal and GitHub never show a dead link.
func TestRelativeLinksResolve(t *testing.T) {
	var pages []string
	for _, pat := range []string{"*.md", "tasks/*.md", "connections/*.md", "templates/*.md", "developing/*.md", "../AGENTS.md", "../README.md"} {
		m, _ := filepath.Glob(pat)
		pages = append(pages, m...)
	}
	if len(pages) < 15 {
		t.Fatalf("only %d pages found", len(pages))
	}
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mdLink.FindAllStringSubmatch(stripCode(string(b)), -1) {
			link := m[1]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "#") || strings.HasPrefix(link, "mailto:") || strings.HasPrefix(link, "$") {
				continue
			}
			target := strings.SplitN(link, "#", 2)[0]
			if _, err := os.Stat(filepath.Join(filepath.Dir(p), target)); err != nil {
				t.Errorf("%s: broken link %q", p, link)
			}
		}
	}
}

// llms.txt links are raw GitHub URLs of files in this repo; each must exist.
func TestLLMsTxtLinksExist(t *testing.T) {
	b, err := os.ReadFile("../llms.txt")
	if err != nil {
		t.Fatal(err)
	}
	const raw = "https://raw.githubusercontent.com/olafkfreund/siphon/main/"
	n := 0
	for _, m := range mdLink.FindAllStringSubmatch(string(b), -1) {
		if path, ok := strings.CutPrefix(m[1], raw); ok {
			n++
			if _, err := os.Stat(filepath.Join("..", path)); err != nil {
				t.Errorf("llms.txt links to missing %s", path)
			}
		}
	}
	if n < 15 {
		t.Fatalf("llms.txt has only %d repo links", n)
	}
}

// The generated files match their generators: run `go generate ./docs`.
func TestGeneratedFilesFresh(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run")
	}
	full, err := exec.Command("go", "run", "./internal/genllms").Output()
	if err != nil {
		t.Fatal(err)
	}
	if have, _ := os.ReadFile("../llms-full.txt"); !bytes.Equal(have, full) {
		t.Error("llms-full.txt is stale: run `go generate ./docs`")
	}
	help, err := exec.Command("go", "run", "../cmd/siphon", "help", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	gen := exec.Command("go", "run", "./internal/gencli")
	gen.Stdin = bytes.NewReader(help)
	cli, err := gen.Output()
	if err != nil {
		t.Fatal(err)
	}
	if have, _ := os.ReadFile("cli.md"); !bytes.Equal(have, cli) {
		t.Error("cli.md is stale: run `go generate ./docs`")
	}
}

// stripCode drops fenced code blocks, whose ](…) text isn't a link.
func stripCode(s string) string {
	var b strings.Builder
	in := false
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			in = !in
			continue
		}
		if !in {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}
