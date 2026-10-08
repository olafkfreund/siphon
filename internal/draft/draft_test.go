package draft

import (
	"slices"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/applyfile"
)

func TestExtractYAML(t *testing.T) {
	for in, want := range map[string]string{
		"Sure!\n```yaml\na: 1\n```\nenjoy":      "a: 1\n",
		"```\nb: 2\n```":                        "b: 2\n",
		"```yml\nc: 3\n```\n```yaml\nd: 4\n```": "c: 3\n",
		"e: 5":                                  "e: 5\n",
	} {
		if got := ExtractYAML(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestTodo(t *testing.T) {
	inv := map[string]any{
		"connections": []map[string]any{{"name": "ollama-local"}},
		"sources":     []map[string]any{{"name": "gh"}},
	}
	items, err := applyfile.Parse([]byte(`sources:
  hook: { type: webhook, signature: token, token_header: X }
  withsecret: { type: webhook, signature: token, token_header: X, secret: "file:/x" }
agents:
  a: { kind: claude, credential: missing, prompt: p, mcp: [gh, nope] }
  b: { kind: model, credential: ollama-local, model: m, prompt: p, approve: false }
rules:
  - { name: r, source: hook, when: "true", action: { agent: a } }
`))
	if err != nil {
		t.Fatal(err)
	}
	todo := strings.Join(Todo(items, inv, nil), "\n")
	for _, want := range []string{"secret sources/hook.secret", "connection missing does not exist", "source nope", "siphon approve"} {
		if !strings.Contains(todo, want) {
			t.Errorf("todo lacks %q:\n%s", want, todo)
		}
	}
	for _, not := range []string{"withsecret", "ollama-local does not", "source gh"} {
		if strings.Contains(todo, not) {
			t.Errorf("todo has %q:\n%s", not, todo)
		}
	}
	if !slices.Equal(WebhookNeedsSecret(items), []string{"sources/hook"}) {
		t.Errorf("%v", WebhookNeedsSecret(items))
	}
	if len(Todo(nil, nil, nil)) != 0 {
		t.Error("an empty file has a to-do")
	}
}

func TestApprovalErrors(t *testing.T) {
	y := "agents:\n  a: { kind: claude, prompt: p, approve: false }\n"
	if e := approvalErrors("summarise my PRs", y); len(e) != 1 {
		t.Errorf("not flagged: %v", e)
	}
	if e := approvalErrors("run it without approval", y); e != nil {
		t.Errorf("flagged though asked: %v", e)
	}
}

func TestMatchTemplatesAndPrompt(t *testing.T) {
	ts := matchTemplates("review every github pull request", 3)
	if len(ts) == 0 || ts[0].Name != "github-pr-review" {
		t.Fatalf("%v", ts)
	}
	if ts = matchTemplates("zzzz qqqq", 3); len(ts) != 1 || ts[0].Name != "webhook-command" {
		t.Fatalf("fallback: %v", ts)
	}
	msgs := Prompt("review every github pull request", map[string]any{"sources": []map[string]any{{"name": "gh"}}})
	if len(msgs) != 2 || msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "agent fields") || !strings.Contains(msgs[1].Content, `"gh"`) {
		t.Fatalf("%+v", msgs)
	}
	if strings.Contains(msgs[0].Content, "routine fields") {
		t.Error("routine table for a request that has no routine")
	}
}

func TestPromptListsCatalogueServices(t *testing.T) {
	sys := Prompt("tell me about prs", map[string]any{})[0].Content
	for _, want := range []string{"siphon connect <id>", "- github (tools, webhooks): sources/<name>, sources/<name>-hooks (optional)", "- aws (", "credentials/<name>"} {
		if !strings.Contains(sys, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}
