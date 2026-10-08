package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"

	siphon "github.com/olafkfreund/siphon"
	"github.com/olafkfreund/siphon/docs"
)

// Help & Docs: the embedded guide rendered in the portal, a live "get
// started" checklist and the template gallery. Everything sits behind the
// portal login. Markdown is rendered without raw HTML, so the pages stay
// CSP-clean (no inline script, style or handler can come out of a page).

const repoBlob = "https://github.com/olafkfreund/siphon/blob/main/"

type helpStep struct {
	Done                            bool
	Title, How, Cmd, Link, LinkText string
}

type helpCard struct{ Title, Desc, Href string }

type helpView struct {
	Steps     []helpStep
	Cards     []helpCard
	DoneCount int
}

type navItem struct {
	Href, Title string
	Current     bool
}

type navGroup struct {
	Title string
	Items []navItem
}

type helpPageView struct {
	Title string
	Body  template.HTML
	Nav   []navGroup
	Raw   string // the page's .md URL
}

type needLink struct{ Ref, Href string }

type tplCard struct {
	docs.Template
	Needs        []needLink
	Print, Apply string
}

type tplGroup struct {
	Category string
	Items    []tplCard
}

type galleryView struct {
	Groups []tplGroup
	Nav    []navGroup
	Total  int
}

var parenNote = regexp.MustCompile(`\s*\([^)]*\)`)

func (s *server) helpRoutes(mux *http.ServeMux) {
	get := func(pattern string, h func(w http.ResponseWriter, r *http.Request, csrf string)) {
		mux.HandleFunc("GET "+pattern, s.portal(h))
	}
	get("/help", func(w http.ResponseWriter, r *http.Request, csrf string) {
		v, err := s.helpView(r)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.page(w, r, "help", view{CSRF: csrf, Help: v})
	})
	get("/help/templates", func(w http.ResponseWriter, r *http.Request, csrf string) {
		s.page(w, r, "helptemplates", view{CSRF: csrf, Gallery: galleryFor()})
	})
	get("/help/{page...}", func(w http.ResponseWriter, r *http.Request, csrf string) {
		name := r.PathValue("page")
		if name == "" {
			http.Redirect(w, r, "/help", http.StatusSeeOther)
			return
		}
		if base, ok := strings.CutSuffix(name, ".md"); ok { // the raw Markdown
			b, ok := docs.Page(base)
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Write(b)
			return
		}
		b, ok := docs.Page(name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.page(w, r, "helppage", view{CSRF: csrf, HelpPage: &helpPageView{
			Title: pageTitle(name, b), Body: renderMarkdown(name, b), Nav: helpNav("/help/" + name), Raw: "/help/" + name + ".md"}})
	})
	raw := func(path string, b []byte) {
		get(path, func(w http.ResponseWriter, _ *http.Request, _ string) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write(b)
		})
	}
	raw("/llms.txt", siphon.LLMsTxt)
	raw("/llms-full.txt", siphon.LLMsFullTxt)
}

// ------------------------------------------------------------ checklist

func (s *server) count(query string) int {
	var n int
	s.Store.DB.QueryRow(query).Scan(&n)
	return n
}

func (s *server) portalURL(r *http.Request) string {
	if u := strings.TrimRight(s.Config().Server.PublicURL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if secureCookie(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *server) helpView(r *http.Request) (*helpView, error) {
	cfg := s.Config()
	v := &helpView{}
	step := func(done bool, title, how, cmd, link, linkText string) {
		v.Steps = append(v.Steps, helpStep{done, title, how, cmd, link, linkText})
		if done {
			v.DoneCount++
		}
	}
	step(len(s.logins())+len(s.modelConns()) > 0, "Connect something to run on",
		"A model endpoint (Ollama, OpenAI-compatible) or a Claude, Codex or agy login.", "siphon connect model ollama", "/connections", "Connections")
	step(len(cfg.Sources) > 0, "Add a source",
		"Where events come from: a webhook, a polled URL or an MCP server. A template sets one up with its rule.", "siphon template", "/sources", "Sources")
	step(len(cfg.Rules) > 0, "Create a rule",
		"When a condition holds, run a command, a routine or an agent. The wizard asks for each part.", "siphon new task", "/rules", "Rules")
	step(s.count(`SELECT COUNT(*) FROM jobs WHERE state IN ('done','failed')`) > 0, "See a job run",
		"Fire the source (or `siphon test <rule> --last`), then look at the job and its output.", "siphon why <rule>", "/jobs", "Jobs")
	step(s.count(`SELECT COUNT(*) FROM config_revision WHERE actor LIKE 'api:cli:%'`) > 0, "Use the CLI",
		"Everything here can be done from a terminal, and an AI assistant can drive it too.", "siphon login "+s.portalURL(r), "/history", "History")
	for _, c := range []helpCard{
		{"Templates", "About 20 ready-made tasks you can copy.", "/help/templates"},
		{"Getting started", "From login to a working agent task in ten minutes.", "/help/getting-started"},
		{"Concepts", "Source, rule, action, agents, connections, approvals.", "/help/concepts"},
		{"How-to guides", "By task: webhooks, thresholds, pull-request agents, routines.", "/help/README"},
		{"Connections", "GitHub, GitLab, AWS, models and logins.", "/help/connections/logins"},
		{"Troubleshooting", "Why a rule didn't fire, and what to run.", "/help/troubleshooting"},
		{"CLI reference", "Every command, flag and exit code.", "/help/cli"},
		{"For AI assistants", "The guide an assistant reads before changing anything.", "/help/llm"},
		{"llms.txt", "The docs index for language models.", "/llms.txt"},
	} {
		if p, ok := strings.CutPrefix(c.Href, "/help/"); ok && p != "templates" {
			if _, ok := docs.Page(p); !ok {
				continue
			}
		}
		v.Cards = append(v.Cards, c)
	}
	return v, nil
}

// ------------------------------------------------------------ navigation

var md = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithParserOptions(parser.WithAutoHeadingID()))

// pageTitle is the page's first heading.
func pageTitle(name string, b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if t, ok := strings.CutPrefix(l, "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	return name
}

func helpNav(current string) []navGroup {
	item := func(href string) (navItem, bool) {
		if href == "/help/templates" {
			return navItem{href, "Templates", current == href}, true
		}
		b, ok := docs.Page(strings.TrimPrefix(href, "/help/"))
		if !ok {
			return navItem{}, false
		}
		return navItem{href, pageTitle(href, b), current == href}, true
	}
	group := func(title string, hrefs ...string) navGroup {
		g := navGroup{Title: title}
		for _, h := range hrefs {
			if it, ok := item(h); ok {
				g.Items = append(g.Items, it)
			}
		}
		return g
	}
	dir := func(d string) (out []string) {
		es, _ := fs.ReadDir(docs.FS(), d)
		for _, e := range es {
			if n, ok := strings.CutSuffix(e.Name(), ".md"); ok && n != "README" {
				out = append(out, "/help/"+d+"/"+n)
			}
		}
		sort.Strings(out)
		return
	}
	groups := []navGroup{
		{Title: "", Items: []navItem{{"/help", "Get started", current == "/help"}}},
		group("Learn", "/help/getting-started", "/help/concepts", "/help/templates", "/help/README"),
		group("How-to", dir("tasks")...),
		group("Connections", dir("connections")...),
		group("Reference", "/help/cli", "/help/configuration", "/help/troubleshooting", "/help/drafting", "/help/llm"),
		group("Developing", dir("developing")...),
	}
	out := groups[:0]
	for _, g := range groups {
		if len(g.Items) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// ------------------------------------------------------------ templates

func needHref(ref string) string {
	kind, name, _ := strings.Cut(ref, "/")
	switch {
	case kind == "credentials":
		return "/connections"
	case kind == "sources" && (strings.HasPrefix(name, "github") || strings.HasPrefix(name, "gitlab") || strings.HasPrefix(name, "aws")):
		return "/services"
	case kind == "sources":
		return "/config/sources/" + url.PathEscape(name)
	}
	return ""
}

func galleryFor() *galleryView {
	g := &galleryView{Nav: helpNav("/help/templates")}
	for _, t := range docs.Templates() {
		c := tplCard{Template: t, Print: "siphon template " + t.Name + " > " + t.Name + ".yaml", Apply: strings.TrimSpace(parenNote.ReplaceAllString(t.Apply, ""))}
		for _, n := range t.Needs {
			c.Needs = append(c.Needs, needLink{n, needHref(n)})
		}
		if len(g.Groups) == 0 || g.Groups[len(g.Groups)-1].Category != t.Category {
			g.Groups = append(g.Groups, tplGroup{Category: t.Category})
		}
		last := &g.Groups[len(g.Groups)-1]
		last.Items = append(last.Items, c)
		g.Total++
	}
	return g
}

// ------------------------------------------------------------ markdown

// renderMarkdown renders a docs page. Raw HTML in the source is dropped by
// goldmark (its safe default), and relative links are pointed at the portal.
func renderMarkdown(page string, src []byte) template.HTML {
	doc := md.Parser().Parse(text.NewReader(src))
	dir := path.Dir(page)
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			l.Destination = []byte(rewriteLink(dir, string(l.Destination)))
		}
		return ast.WalkContinue, nil
	})
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return template.HTML("<p>This page could not be rendered.</p>")
	}
	return template.HTML(buf.String())
}

// rewriteLink points a relative link of a page in dir at the portal: other
// pages at /help/<page>, templates at the gallery, anything else in the
// repository at GitHub. Absolute URLs and fragments are left alone.
func rewriteLink(dir, dest string) string {
	if dest == "" || strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "/") || regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`).MatchString(dest) {
		return dest
	}
	p, frag, _ := strings.Cut(dest, "#")
	if frag != "" {
		frag = "#" + frag
	}
	full := path.Join("docs", dir, p) // repository path
	rel := strings.TrimPrefix(full, "docs/")
	switch {
	case full == "docs":
		return "/help/README" + frag
	case strings.HasPrefix(rel, "templates/") && strings.HasSuffix(rel, ".yaml"):
		return "/help/templates#" + strings.TrimSuffix(strings.TrimPrefix(rel, "templates/"), ".yaml")
	case rel == "templates/README.md":
		return "/help/templates"
	case strings.HasPrefix(full, "docs/") && strings.HasSuffix(rel, ".md"):
		if _, ok := docs.Page(strings.TrimSuffix(rel, ".md")); ok {
			return "/help/" + strings.TrimSuffix(rel, ".md") + frag
		}
	}
	return repoBlob + full + frag
}
