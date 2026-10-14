package web

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/config"
)

const slice3Cfg = `server: { sandbox: none, db: DIR/s.db }
credentials:
  cl: { provider: claude }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
agents:
  helper: { kind: claude, credential: cl, prompt: hi }
routines:
  deploy:
    steps:
      - { id: a, cmd: [echo, one] }
      - { id: b, agent: helper, approve: true }
rules:
  - { name: r1, source: gh, when: "true", cooldown: 1m, action: { agent: helper } }
  - { name: r2, source: gh, when: "true", cooldown: 1m, action: { routine: deploy } }
`

func TestConfigListsAgentsRoutines(t *testing.T) {
	ce := newCfgEnvFile(t, slice3Cfg)
	a := ce.get("/config/agents").Body.String()
	for _, want := range []string{`aria-current="page"`, "item-card row", "claude", "credential <code>cl</code>", "used by 1 rule"} {
		if !strings.Contains(a, want) {
			t.Errorf("agents: missing %q", want)
		}
	}
	r := ce.get("/config/routines").Body.String()
	for _, want := range []string{"2 steps", "steps-mini", "asks approval", "used by 1 rule"} {
		if !strings.Contains(r, want) {
			t.Errorf("routines: missing %q", want)
		}
	}
	if strings.Contains(r, "filter-bar") {
		t.Error("filter bar on a short list")
	}
	if e := newCfgEnv(t).get("/config/agents").Body.String(); !strings.Contains(e, `href="/help/templates"`) || !strings.Contains(e, `href="/config/agents/new"`) {
		t.Error("empty state links")
	}
	// Reset asks first.
	if w := ce.saveYAML("rules", "r1", `{source: gh, when: "false", action: {cmd: [echo]}}`); w.Code != 303 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if b := ce.get("/config/rules").Body.String(); !strings.Contains(b, `data-confirm="Reset r1 to the file version?"`) {
		t.Error("reset lacks data-confirm")
	}
}

func TestServicesKeepCategoryAndViewerGates(t *testing.T) {
	ce := newCfgEnv(t)
	b := ce.get("/services?cat=cloud").Body.String()
	in, hid := strings.Index(b, `id="service-results"`), strings.Index(b, `name="cat" value="cloud" form="service-search"`)
	if in < 0 || hid < in {
		t.Errorf("hidden cat must sit inside #service-results (%d, %d)", in, hid)
	}
	if !strings.Contains(b, `class="chip" href`) || !strings.Contains(b, `id="service-search"`) {
		t.Error("chips or form id missing")
	}
	if !strings.Contains(ce.get("/services").Body.String(), `<svg`) || !strings.Contains(ce.get("/services").Body.String(), `href="/services/github"`) {
		t.Error("admin: available pill icon or Connect missing")
	}
	c, _ := ce.asRole(roleViewer, "v")
	v := ce.do("GET", "/services", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if strings.Contains(v, `href="/services/github"`) || !strings.Contains(v, "An admin connects services") {
		t.Error("viewer sees Connect")
	}
}

func TestConnectionsDottedNameAndViewer(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Cfg.Credentials = map[string]*config.Credential{"a.b": {Provider: "ollama", URL: "http://127.0.0.1:11434"}}
		o.Cfg.Agents = map[string]*config.Agent{"helper": {Kind: "model", Credential: "a.b"}}
	})
	b := e.page("/connections")
	if !strings.Contains(b, `hx-target="next .conn-test"`) || strings.Contains(b, `test-a.b`) {
		t.Error("test target must be relative, with no id built from the name")
	}
	if !strings.Contains(b, "used by 1 agent") || !strings.Contains(b, `id="add-model"`) {
		t.Error("used-by or admin add form missing")
	}
	c, _ := e.asRole(roleViewer, "v")
	v := e.do("GET", "/connections", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if strings.Contains(v, `id="add-model"`) || strings.Contains(v, `id="add"`) || strings.Contains(v, `href="#add`) {
		t.Error("viewer sees add controls")
	}
}

func TestStyleRules(t *testing.T) {
	b, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if regexp.MustCompile(`(?m)^\.g3 \{ grid-template-columns: repeat\(3`).MatchString(css) {
		t.Error("the global .g3 three-column override is back")
	}
	if !strings.Contains(css, ":not(:has([name=provider][value=agy]:checked)):has([name=kind][value=apikey]:checked) .h-apikey") {
		t.Error("API-key help must be excluded for agy")
	}
	for _, want := range []string{".item-card.is-ok::before", ".conn-test:empty", ".steps-mini"} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %s", want)
		}
	}
}
