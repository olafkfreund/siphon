package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/siphon/internal/store"
)

type mcpEnv struct {
	*cliEnv
	cs *mcp.ClientSession
}

// mcpSession serves siphon's MCP tools over in-memory transports against the real API.
func newMCPEnv(t *testing.T, allowWrite, allowSecrets bool) *mcpEnv {
	e := newCLIEnv(t)
	t.Setenv("SIPHON_URL", e.url)
	t.Setenv("SIPHON_TOKEN_FILE", e.tokf)
	t.Setenv("USER", "olaf")
	c := &cli{in: strings.NewReader(""), out: &strings.Builder{}, errw: &strings.Builder{}, g: globals{output: "text"}, actor: "cli:olaf:mcp"}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := newMCPServer(c, allowWrite, allowSecrets).Connect(ctx, srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return &mcpEnv{e, cs}
}

// call runs a tool and returns its text and whether it was an error result.
func (m *mcpEnv) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := m.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s: no content", tool)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

const mcpRule = "rules:\n  - { name: via-mcp, source: gh, when: \"true\", action: { cmd: [echo, hi] } }\n"

func TestMCPToolList(t *testing.T) {
	m := newMCPEnv(t, false, false)
	res, err := m.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tool := range res.Tools {
		have[tool.Name] = true
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("%s: no description or schema", tool.Name)
		}
		if n := strings.ToLower(tool.Name + tool.Description); strings.Contains(tool.Name, "approve") || strings.Contains(tool.Name, "deny") || strings.Contains(tool.Name, "decide") || strings.Contains(n, "approve a") {
			t.Errorf("a deciding tool: %s", tool.Name)
		}
	}
	for _, want := range []string{"inventory", "get", "explain", "template", "test", "why", "jobs", "job", "status", "apply", "delete", "guide"} {
		if !have[want] {
			t.Errorf("tool %s missing", want)
		}
	}
	if len(res.Tools) != 12 {
		t.Errorf("%d tools: %v", len(res.Tools), have)
	}
}

func TestMCPReadTools(t *testing.T) {
	m := newMCPEnv(t, false, false)
	if out, isErr := m.call(t, "get", map[string]any{"kind": "rules"}); isErr || !strings.Contains(out, `"r1"`) {
		t.Fatalf("get: %s", out)
	}
	if out, _ := m.call(t, "get", map[string]any{"kind": "rules", "name": "r1"}); !strings.Contains(out, "source: gh") {
		t.Fatalf("get one: %s", out)
	}
	var why map[string]any
	out, _ := m.call(t, "why", map[string]any{"rule": "r1"})
	if json.Unmarshal([]byte(out), &why) != nil || why["reasons"] == nil {
		t.Fatalf("why: %s", out)
	}
	if out, _ = m.call(t, "template", nil); !strings.Contains(out, "github-pr-review") || strings.Contains(out, `"yaml"`) {
		t.Fatalf("template list: %.200s", out)
	}
	if out, _ = m.call(t, "template", map[string]any{"name": "disk-full"}); !strings.Contains(out, `"yaml"`) || !strings.Contains(out, `"apply"`) {
		t.Fatalf("template one: %.200s", out)
	}
	if out, _ = m.call(t, "explain", map[string]any{"kind": "rule"}); !strings.Contains(out, "action.cmd") {
		t.Fatalf("explain: %.200s", out)
	}
	if out, _ = m.call(t, "inventory", nil); !strings.Contains(out, `"sources"`) {
		t.Fatalf("inventory: %.200s", out)
	}
	if out, _ = m.call(t, "status", nil); !strings.Contains(out, `"approvals"`) {
		t.Fatalf("status: %s", out)
	}
	if out, _ = m.call(t, "guide", nil); len(out) < 100 {
		t.Fatalf("guide: %q", out)
	}
	if out, _ = m.call(t, "test", map[string]any{"rule": "r1", "event": map[string]any{"v": 1}}); !strings.Contains(out, `"fires"`) {
		t.Fatalf("test: %s", out)
	}
	store.SetSourceEvent(m.st.DB, "gh", map[string]any{"v": 2})
	if out, isErr := m.call(t, "test", map[string]any{"rule": "r1", "use_last": true}); isErr || !strings.Contains(out, `"fires"`) {
		t.Fatalf("test use_last: %s", out)
	}
	if out, _ = m.call(t, "jobs", map[string]any{"state": "queued", "limit": 5}); !strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("jobs: %s", out)
	}
	if out, isErr := m.call(t, "job", map[string]any{"id": 999}); !isErr || !strings.Contains(out, `"hint"`) {
		t.Fatalf("missing job: %s", out)
	}
}

func TestMCPErrorsCarryAHint(t *testing.T) {
	m := newMCPEnv(t, true, false)
	out, isErr := m.call(t, "get", map[string]any{"kind": "rules", "name": "nope"})
	var e struct {
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
		Hint   string   `json:"hint"`
	}
	if !isErr || json.Unmarshal([]byte(out), &e) != nil || e.Error == "" || e.Hint == "" || e.Errors == nil {
		t.Fatalf("not found: %v %s", isErr, out)
	}
	out, isErr = m.call(t, "apply", map[string]any{"yaml": "rules:\n  - { name: b, source: nope, when: \"true\", action: { cmd: [x] } }\n"})
	if !isErr || json.Unmarshal([]byte(out), &e) != nil || len(e.Errors) == 0 || !strings.Contains(e.Hint, "fix the errors") {
		t.Fatalf("invalid: %v %s", isErr, out)
	}
	if out, isErr = m.call(t, "get", map[string]any{"kind": "widgets"}); !isErr || !strings.Contains(out, "kinds:") {
		t.Fatalf("bad kind: %s", out)
	}
	if out, isErr = m.call(t, "apply", map[string]any{"yaml": "server: {}\n"}); !isErr {
		t.Fatalf("fixed section: %s", out)
	}
}

func TestMCPWriteGate(t *testing.T) {
	m := newMCPEnv(t, false, false)
	revs := func() int { r, _ := store.Revisions(m.st.DB, 10); return len(r) }
	out, isErr := m.call(t, "apply", map[string]any{"yaml": mcpRule})
	if isErr || !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, "--allow-write") || !strings.Contains(out, `"diff"`) {
		t.Fatalf("apply without the flag: %s", out)
	}
	// asking for a write explicitly is still only a dry run
	if out, _ = m.call(t, "apply", map[string]any{"yaml": mcpRule, "dry_run": false}); !strings.Contains(out, `"dry_run": true`) {
		t.Fatalf("explicit write: %s", out)
	}
	if out, _ = m.call(t, "delete", map[string]any{"kind": "rules", "name": "r1"}); !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, "--allow-write") {
		t.Fatalf("delete without the flag: %s", out)
	}
	if items, _ := store.ConfigItems(m.st.DB); len(items) != 0 || revs() != 0 {
		t.Fatal("a gated write committed")
	}
	if _, isErr = m.call(t, "get", map[string]any{"kind": "rules", "name": "r1"}); isErr {
		t.Fatal("r1 vanished")
	}
	// an explicit dry run has no note
	if out, _ = m.call(t, "apply", map[string]any{"yaml": mcpRule, "dry_run": true}); strings.Contains(out, "note") {
		t.Fatalf("note on an asked-for dry run: %s", out)
	}
}

func TestMCPWriteAllowed(t *testing.T) {
	m := newMCPEnv(t, true, false)
	out, isErr := m.call(t, "apply", map[string]any{"yaml": mcpRule})
	if isErr || !strings.Contains(out, `"dry_run": false`) || !strings.Contains(out, `"applied": true`) {
		t.Fatalf("apply: %s", out)
	}
	if out, _ = m.call(t, "get", map[string]any{"kind": "rules", "name": "via-mcp"}); !strings.Contains(out, "source: gh") {
		t.Fatalf("not applied: %s", out)
	}
	revs, _ := store.Revisions(m.st.DB, 1)
	if len(revs) != 1 || revs[0].Actor != "api:cli:olaf:mcp" {
		t.Fatalf("actor: %+v", revs)
	}
	if out, isErr = m.call(t, "delete", map[string]any{"kind": "rules", "name": "via-mcp", "dry_run": true}); isErr || !strings.Contains(out, `"dry_run": true`) {
		t.Fatalf("delete dry: %s", out)
	}
	if out, isErr = m.call(t, "delete", map[string]any{"kind": "rules", "name": "via-mcp"}); isErr || !strings.Contains(out, `"dry_run": false`) {
		t.Fatalf("delete: %s", out)
	}
	if _, isErr = m.call(t, "get", map[string]any{"kind": "rules", "name": "via-mcp"}); !isErr {
		t.Fatal("not deleted")
	}
}

func TestMCPSecretsNeedBothFlags(t *testing.T) {
	const hook = "sources:\n  hk: { type: webhook, signature: github }\n"
	for _, c := range []struct{ write, secrets bool }{{false, false}, {true, false}, {false, true}} {
		m := newMCPEnv(t, c.write, c.secrets)
		out, isErr := m.call(t, "apply", map[string]any{"yaml": hook, "secrets": map[string]string{"sources/hk.secret": "mcp-SECRET"}})
		if !isErr || !strings.Contains(out, "--secret") || !strings.Contains(out, `"hint"`) || strings.Contains(out, "mcp-SECRET") {
			t.Fatalf("%+v: %s", c, out)
		}
		if items, _ := store.ConfigItems(m.st.DB); len(items) != 0 {
			t.Fatalf("%+v: stored despite the refusal", c)
		}
	}
	m := newMCPEnv(t, true, true)
	out, isErr := m.call(t, "apply", map[string]any{"yaml": hook, "secrets": map[string]string{"sources/hk.secret": "mcp-SECRET"}})
	if isErr || !strings.Contains(out, `"applied": true`) || strings.Contains(out, "mcp-SECRET") {
		t.Fatalf("both flags: %s", out)
	}
}

func TestMCPHelpExample(t *testing.T) {
	e := newCLIEnv(t)
	out := e.ok("help", "mcp")
	if !strings.Contains(out, "claude mcp add siphon -- siphon mcp") || !strings.Contains(out, "-allow-write") || !strings.Contains(out, "-allow-secrets") {
		t.Fatalf("help mcp: %s", out)
	}
	if code, _, _ := e.do("mcp", "--allow-secrets"); code != 2 {
		t.Fatalf("--allow-secrets alone: %d", code)
	}
}
