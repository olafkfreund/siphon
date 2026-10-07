package web

import (
	"html"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	siphon "github.com/olafkfreund/siphon"
	"github.com/olafkfreund/siphon/docs"
	"github.com/olafkfreund/siphon/internal/store"
)

var (
	inlineStyle = regexp.MustCompile(`(?i)<style|<[^>]*\sstyle\s*=|<[^>]*\son[a-z]+\s*=`)
	scriptTag   = regexp.MustCompile(`(?i)<script[^>]*>`)
)

// cspClean fails if html has an inline style, handler or script.
func cspClean(t *testing.T, what, html string) {
	t.Helper()
	if loc := inlineStyle.FindStringIndex(html); loc != nil {
		t.Errorf("%s: inline style or handler %q", what, html[max(0, loc[0]-80):min(len(html), loc[1]+40)])
	}
	for _, tag := range scriptTag.FindAllString(html, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("%s: inline script %q", what, tag)
		}
	}
}

func TestHelpNeedsLogin(t *testing.T) {
	ce := newCfgEnv(t)
	for _, p := range []string{"/help", "/help/templates", "/help/concepts", "/help/concepts.md", "/llms.txt", "/llms-full.txt", "/help/nope"} {
		if w := ce.do("GET", p, nil, nil); w.Code != 303 || w.Header().Get("Location") != "/login" {
			t.Errorf("%s without a session: %d", p, w.Code)
		}
	}
}

func TestHelpChecklistTicksFromState(t *testing.T) {
	ce := newCfgEnv(t) // has a source and a rule, nothing else
	w := ce.get("/help")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "2 of 5 done") || strings.Count(body, `class="stepmark done"`) != 2 {
		t.Fatalf("start: %d\n%s", w.Code, body)
	}
	for _, want := range []string{"siphon connect model ollama", "siphon new task", `href="/connections"`, `href="/sources"`, `href="/rules"`, `href="/jobs"`, "siphon login http://example.com", `data-action="copy" data-target="step-0"`, `href="/help/templates"`, `href="/llms.txt"`} {
		if !strings.Contains(body, want) {
			t.Errorf("checklist lacks %q", want)
		}
	}
	// seed: a connection, a finished job, a revision made by the CLI
	if w := ce.api("POST", "/api/connections/logins", `{"name":"k1","provider":"claude","kind":"apikey","value":"sk-x"}`); w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	if body = ce.get("/help").Body.String(); !strings.Contains(body, "3 of 5 done") {
		t.Fatalf("after a connection:\n%s", body)
	}
	tx, _ := ce.st.DB.Begin()
	store.InsertJob(tx, store.Job{Rule: "r1", ActionJSON: "{}", State: "done", RunAfter: time.Now()}, time.Now())
	tx.Commit()
	if body = ce.get("/help").Body.String(); !strings.Contains(body, "4 of 5 done") {
		t.Fatalf("after a job:\n%s", body)
	}
	if w := ce.apiAs("PUT", "/api/config/rules/viacli", `{"yaml":"source: gh\nwhen: \"true\"\naction: {cmd: [echo]}\n"}`, "cli:olaf"); w.Code != 200 {
		t.Fatalf("cli revision: %d %s", w.Code, w.Body.String())
	}
	if body = ce.get("/help").Body.String(); !strings.Contains(body, "5 of 5 done") || strings.Count(body, `class="stepmark done"`) != 5 {
		t.Fatalf("all done:\n%s", body)
	}
	// a queued job is not "has run"
	ce2 := newCfgEnv(t)
	tx, _ = ce2.st.DB.Begin()
	store.InsertJob(tx, store.Job{Rule: "r1", ActionJSON: "{}", State: "queued", RunAfter: time.Now()}, time.Now())
	tx.Commit()
	if body = ce2.get("/help").Body.String(); strings.Contains(body, "3 of 5 done") {
		t.Fatal("a queued job ticked 'a job has run'")
	}
}

func TestHelpPagesRenderCSPClean(t *testing.T) {
	ce := newCfgEnv(t)
	n := 0
	fs.WalkDir(docs.FS(), ".", func(p string, d fs.DirEntry, _ error) error {
		name, ok := strings.CutSuffix(p, ".md")
		if !ok || d.IsDir() {
			return nil
		}
		n++
		w := ce.get("/help/" + name)
		if w.Code != 200 {
			t.Errorf("%s: %d", name, w.Code)
			return nil
		}
		body := w.Body.String()
		cspClean(t, name, body)
		if !strings.Contains(body, `class="doc"`) || !strings.Contains(body, `class="docnav"`) || !strings.Contains(body, "View as Markdown") {
			t.Errorf("%s: not a doc page", name)
		}
		raw := ce.get("/help/" + name + ".md")
		if raw.Code != 200 || raw.Header().Get("Content-Type") != "text/markdown; charset=utf-8" {
			t.Errorf("%s.md: %d %q", name, raw.Code, raw.Header().Get("Content-Type"))
		}
		return nil
	})
	if n < 10 {
		t.Fatalf("only %d pages rendered", n)
	}
	// the nav lists the guide and marks the current page
	body := ce.get("/help/concepts").Body.String()
	for _, want := range []string{`href="/help/getting-started"`, `href="/help/templates"`, `aria-current="page"`, `href="/help/llm"`} {
		if !strings.Contains(body, want) {
			t.Errorf("nav lacks %q", want)
		}
	}
}

func TestMarkdownEscapesRawHTML(t *testing.T) {
	out := string(renderMarkdown("x", []byte("# Hi\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(2)>\n\ninline <b onclick=alert(3)>b</b> text\n\n[bad](javascript:alert(4)) and [ok](https://example.com)\n\n```\n<script>in code</script>\n```\n")))
	cspClean(t, "markdown", out)
	for _, bad := range []string{"<script>alert", "onerror=", "onclick=", "javascript:"} {
		if strings.Contains(out, bad) {
			t.Errorf("output holds %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "&lt;script&gt;in code") || !strings.Contains(out, `href="https://example.com"`) || !strings.Contains(out, `<h1 id="hi">Hi</h1>`) {
		t.Errorf("code or links lost:\n%s", out)
	}
}

func TestHelpLinkRewriting(t *testing.T) {
	for _, tc := range []struct{ dir, in, want string }{
		{".", "getting-started.md", "/help/getting-started"},
		{".", "getting-started.md#next", "/help/getting-started#next"},
		{"tasks", "../concepts.md", "/help/concepts"},
		{"tasks", "webhook-command.md", "/help/tasks/webhook-command"},
		{".", "tasks/routine.md", "/help/tasks/routine"},
		{".", "templates/disk-full.yaml", "/help/templates#disk-full"},
		{"tasks", "../templates/disk-full.yaml", "/help/templates#disk-full"},
		{"templates", "disk-full.yaml", "/help/templates#disk-full"},
		{".", "templates/README.md", "/help/templates"},
		{".", "README.md", "/help/README"},
		{".", "../README.md", repoBlob + "README.md"},
		{"tasks", "../../AGENTS.md", repoBlob + "AGENTS.md"},
		{"developing", "../../internal/web/web.go", repoBlob + "internal/web/web.go"},
		{".", "nope.md", repoBlob + "docs/nope.md"},
		{".", "https://example.com/x", "https://example.com/x"},
		{".", "mailto:a@b.c", "mailto:a@b.c"},
		{".", "#frag", "#frag"},
		{".", "/jobs", "/jobs"},
	} {
		if got := rewriteLink(tc.dir, tc.in); got != tc.want {
			t.Errorf("%s + %s: %s, want %s", tc.dir, tc.in, got, tc.want)
		}
	}
	// and in a rendered page
	out := string(renderMarkdown("tasks/x", []byte("[a](../concepts.md) [b](../templates/disk-full.yaml) [c](../../AGENTS.md)")))
	for _, want := range []string{`href="/help/concepts"`, `href="/help/templates#disk-full"`, `href="` + repoBlob + `AGENTS.md"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered links lack %q:\n%s", want, out)
		}
	}
}

func TestHelpGallery(t *testing.T) {
	ce := newCfgEnv(t)
	w := ce.get("/help/templates")
	body := w.Body.String()
	ts := docs.Templates()
	if w.Code != 200 || strings.Count(body, `class="card tpl"`) != len(ts) {
		t.Fatalf("%d, %d cards for %d templates", w.Code, strings.Count(body, `class="card tpl"`), len(ts))
	}
	cspClean(t, "gallery", body)
	for _, tpl := range ts {
		for _, want := range []string{`id="` + tpl.Name + `"`, `id="y-` + tpl.Name + `"`, `data-target="y-` + tpl.Name + `"`, "siphon template " + tpl.Name + " &gt; " + tpl.Name + ".yaml", `id="a-` + tpl.Name + `"`, `id="p-` + tpl.Name + `"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: gallery lacks %q", tpl.Name, want)
			}
		}
		if m := regexp.MustCompile(`id="a-` + regexp.QuoteMeta(tpl.Name) + `">([^<]*)<`).FindStringSubmatch(body); m == nil || strings.Contains(m[1], "(") || !strings.HasPrefix(m[1], "siphon apply") {
			t.Errorf("%s: the copyable apply command is %q", tpl.Name, m)
		}
		if !strings.Contains(body, html.EscapeString(tpl.Category)) {
			t.Errorf("%s: category %q missing", tpl.Name, tpl.Category)
		}
	}
	// the YAML is escaped, needs link to the connection pages, the apply line has no hint
	for _, want := range []string{`href="/connections"`, `href="/services"`, "&lt;siphon&gt;", "Connect first:", "Secrets to pass:"} {
		if !strings.Contains(body, want) {
			t.Errorf("gallery lacks %q", want)
		}
	}
	// the order is the categories'
	last := -1
	for _, c := range docs.Categories {
		i := strings.Index(body, `id="cat-`+html.EscapeString(c))
		if i >= 0 && i < last {
			t.Errorf("category %q out of order", c)
		}
		if i >= 0 {
			last = i
		}
	}
}

func TestHelpRawAndMissing(t *testing.T) {
	ce := newCfgEnv(t)
	for p, want := range map[string][]byte{"/llms.txt": siphon.LLMsTxt, "/llms-full.txt": siphon.LLMsFullTxt} {
		w := ce.get(p)
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Body.String() != string(want) || len(want) == 0 {
			t.Errorf("%s: %d %q", p, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, p := range []string{"/help/nope", "/help/nope.md", "/help/tasks/nope", "/help/templates/disk-full.yaml", "/help/docs", "/help/..%2fREADME"} {
		if w := ce.get(p); w.Code != 404 && w.Code != 301 && w.Code != 307 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := ce.get("/help/"); w.Code != 303 || w.Header().Get("Location") != "/help" {
		t.Errorf("/help/: %d", w.Code)
	}
	// the sidebar has the entry on every page
	if body := ce.get("/").Body.String(); !strings.Contains(body, `href="/help"`) || !strings.Contains(body, "Help &amp; Docs") {
		t.Error("no Help & Docs in the sidebar")
	}
}
