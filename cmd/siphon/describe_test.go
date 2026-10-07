package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/docs"
	"github.com/olafkfreund/siphon/internal/config"
)

// baseConfig provides what the templates need: the connections and sources
// their `needs` headers name, and the aws allowlists.
const baseConfig = `server: { sandbox: none, db: DIR/s.db, aws: { profiles: [p1] }, models: { private_endpoints: ["127.0.0.1:11434"] }, mcp_packages: { aws-cloudwatch: { command: [cw], hosts: ['logs.{region}.amazonaws.com'], env: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION, AWS_DEFAULT_REGION, AWS_EC2_METADATA_DISABLED, AWS_CONFIG_FILE, AWS_SHARED_CREDENTIALS_FILE] } } }
credentials:
  claude-max: { provider: claude }
  ollama-local: { provider: ollama, url: "http://127.0.0.1:11434" }
  aws: { provider: aws, region: eu-west-1, profile: p1 }
sources:
  github: { type: mcp, url: "https://api.githubcopilot.com/mcp/", auth: { bearer: env:AGW_HOOK }, read: { tool: get_me }, poll: 24h }
  github-hooks: { type: webhook, signature: github, secret: env:AGW_HOOK }
  gitlab-hooks: { type: webhook, signature: token, token_header: X-Gitlab-Token, secret: env:AGW_HOOK }
  aws-cloudwatch: { type: mcp, package: aws-cloudwatch, aws: aws }
  aws-hooks: { type: webhook, signature: token, token_header: X-Siphon-Key, secret: env:AGW_HOOK }
rules:
  - { name: r1, source: github-hooks, when: "true", action: { cmd: [echo, one] } }
`

func TestTemplatesValidate(t *testing.T) {
	e := newCLIEnvFile(t, baseConfig)
	secret := e.write("dummy.secret", "dummy-secret-value\n")
	ts := docs.Templates()
	if len(ts) < 20 {
		t.Fatalf("%d templates", len(ts))
	}
	for _, tpl := range ts {
		t.Run(tpl.Name, func(t *testing.T) {
			f := e.write(tpl.Name+".yaml", tpl.YAML)
			args := []string{"apply", "-f", f, "--dry-run"}
			for _, s := range tpl.Secrets {
				args = append(args, "--secret", s+"=@"+secret)
			}
			code, out, er := e.do(args...)
			if code != 0 {
				t.Fatalf("exit %d\n%s\n%s\nneeds %v secrets %v", code, out, er, tpl.Needs, tpl.Secrets)
			}
		})
	}
	// nothing was stored by any dry run
	if out := e.ok("history", "-o", "json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("history: %s", out)
	}
}

func TestTemplateCommand(t *testing.T) {
	e := newCLIEnv(t)
	out := e.ok("template")
	if !strings.Contains(out, "github-pr-review") || !strings.Contains(out, "Code review & CI") {
		t.Fatalf("list: %s", out)
	}
	var list []docs.Template
	if json.Unmarshal([]byte(e.ok("template", "-o", "json")), &list) != nil || len(list) < 20 || list[0].YAML != "" || list[0].Title == "" {
		t.Fatalf("list json: %+v", list[:1])
	}
	raw := e.ok("template", "disk-full")
	if !strings.HasPrefix(raw, "# title:") || !strings.Contains(raw, "sources:") {
		t.Fatalf("raw: %s", raw)
	}
	if e.ok("example", "disk-full") != raw {
		t.Fatal("alias differs")
	}
	var one docs.Template
	if json.Unmarshal([]byte(e.ok("template", "disk-full", "-o", "json")), &one) != nil || one.YAML != raw || one.Apply == "" {
		t.Fatalf("one json: %+v", one)
	}
	code, _, er := e.do("template", "github-review")
	if code != 4 || !strings.Contains(er, "did you mean") || !strings.Contains(er, "github-pr-review") {
		t.Fatalf("unknown: %d %s", code, er)
	}
	if code, _, er = e.do("template", "zzzz"); code != 4 || !strings.Contains(er, "siphon template") {
		t.Fatalf("no match: %d %s", code, er)
	}
	// the printed template applies
	e.stdin = raw
	e.ok("apply", "-f", "-", "--dry-run")
}

func TestExplainCoversSchema(t *testing.T) {
	e := newCLIEnv(t)
	raw, _ := config.Schema()
	var root map[string]any
	json.Unmarshal(raw, &root)
	for kind, k := range explainKinds {
		var out struct {
			Kind   string  `json:"kind"`
			Fields []field `json:"fields"`
		}
		if json.Unmarshal([]byte(e.ok("explain", kind, "-o", "json")), &out) != nil || out.Kind != kind || len(out.Fields) == 0 {
			t.Fatalf("%s: %+v", kind, out)
		}
		have := map[string]field{}
		for _, f := range out.Fields {
			have[f.Path] = f
			if f.Description == "" || f.Type == "" || f.Enum == nil {
				t.Errorf("%s.%s: incomplete %+v", kind, f.Path, f)
			}
		}
		// every field of the schema is explained, and every hint is for a real field
		node := sub(sub(sub(root, "properties"), k.plural), "additionalProperties")
		if kind == "rule" {
			node = sub(sub(sub(root, "properties"), "rules"), "items")
		}
		for name := range sub(node, "properties") {
			if _, ok := have[name]; !ok {
				t.Errorf("%s: schema field %s is not explained", kind, name)
			}
		}
		for path := range k.hints {
			if _, ok := have[path]; !ok {
				t.Errorf("%s: hint for %s, which is not in the schema", kind, path)
			}
		}
	}
	// the plural works, and dotted paths, enums and requireds show up
	out := e.ok("explain", "rules")
	for _, want := range []string{"action.cmd", "required", "[edge|each]", "(default edge)", "duration"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain rules lacks %q:\n%s", want, out)
		}
	}
	if out = e.ok("explain", "source"); !strings.Contains(out, "read.tool") || !strings.Contains(out, "[github|sha256|token|standard-webhooks]") {
		t.Errorf("explain source:\n%s", out)
	}
	if out = e.ok("explain", "routine"); !strings.Contains(out, "steps[].retry.attempts") {
		t.Errorf("explain routine:\n%s", out)
	}
	if code, _, _ := e.do("explain", "widget"); code != 2 {
		t.Fatalf("unknown kind: %d", code)
	}
	if code, _, _ := e.do("explain"); code != 2 {
		t.Fatalf("no kind: %d", code)
	}
}

func TestInventoryHasNoSecrets(t *testing.T) {
	t.Setenv("AGW_GH_TOKEN", "ghp_TOPSECRET-token-value")
	e := newCLIEnvFile(t, `server: { sandbox: none, db: DIR/s.db, aws: { profiles: [p1], role_arns: ["arn:aws:iam::123456789012:role/ro"] }, models: { private_endpoints: ["127.0.0.1:11434"] }, services: { private_endpoints: ["gitlab.lan:443"] }, mcp_packages: { github: { command: [gh], env: [GITHUB_PERSONAL_ACCESS_TOKEN] } } }
credentials:
  claude-max: { provider: claude }
  keyed: { provider: claude, api_key: env:AGW_GH_TOKEN }
  ollama-local: { provider: ollama, url: "http://127.0.0.1:11434" }
sources:
  gh: { type: mcp, package: github, env: { GITHUB_PERSONAL_ACCESS_TOKEN: env:AGW_GH_TOKEN }, read: { tool: get_me }, poll: 1h }
  hook: { type: webhook, secret: env:AGW_GH_TOKEN, signature: github }
agents:
  helper: { kind: model, credential: ollama-local, model: m, prompt: p }
routines:
  rt: { steps: [{ id: a, cmd: [echo] }] }
rules:
  - { name: r1, source: hook, when: "true", action: { cmd: [echo] } }
`)
	e.ok("disable", "r1")
	out := e.ok("inventory", "-o", "json")
	if strings.Contains(out, "TOPSECRET") || strings.Contains(out, "env:AGW") {
		t.Fatalf("inventory leaks: %s", out)
	}
	var inv struct {
		Sources     []map[string]any `json:"sources"`
		Rules       []map[string]any `json:"rules"`
		Agents      []map[string]any `json:"agents"`
		Routines    []map[string]any `json:"routines"`
		Connections []map[string]any `json:"connections"`
		Packages    []struct {
			Name string   `json:"name"`
			Env  []string `json:"env"`
		} `json:"mcp_packages"`
		Server struct {
			AWS struct {
				Profiles []string `json:"profiles"`
			} `json:"aws"`
			Models struct {
				PrivateEndpoints []string `json:"private_endpoints"`
			} `json:"models"`
			Services struct {
				PrivateEndpoints []string `json:"private_endpoints"`
			} `json:"services"`
		} `json:"server"`
	}
	if err := json.Unmarshal([]byte(out), &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Sources) != 2 || inv.Sources[0]["name"] != "gh" || inv.Sources[0]["type"] != "mcp" || len(inv.Connections) != 3 ||
		inv.Agents[0]["kind"] != "model" || inv.Agents[0]["credential"] != "ollama-local" || inv.Rules[0]["enabled"] != false || len(inv.Routines) != 1 {
		t.Fatalf("%s", out)
	}
	if len(inv.Packages) != 1 || inv.Packages[0].Name != "github" || inv.Packages[0].Env[0] != "GITHUB_PERSONAL_ACCESS_TOKEN" {
		t.Fatalf("packages: %+v", inv.Packages)
	}
	if inv.Server.AWS.Profiles[0] != "p1" || inv.Server.Models.PrivateEndpoints[0] != "127.0.0.1:11434" || inv.Server.Services.PrivateEndpoints[0] != "gitlab.lan:443" {
		t.Fatalf("server: %+v", inv.Server)
	}
	for _, c := range inv.Connections {
		if c["name"] == "keyed" && c["status"] != "apikey" {
			t.Errorf("keyed: %v", c)
		}
	}
	if txt := e.ok("inventory"); !strings.Contains(txt, "sources: gh mcp, hook webhook") || strings.Contains(txt, "TOPSECRET") {
		t.Fatalf("text: %s", txt)
	}
}

func TestGuidePrintsLLMDoc(t *testing.T) {
	e := newCLIEnv(t)
	want, _ := os.ReadFile(filepath.Join("..", "..", "docs", "llm.md"))
	if out := e.ok("guide"); len(out) == 0 || out != string(want) {
		t.Fatalf("guide differs from docs/llm.md (%d vs %d bytes)", len(out), len(want))
	}
}
