package action

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRunAgent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	stub, err := filepath.Abs("testdata/stub-runner.sh")
	if err != nil {
		t.Fatal(err)
	}
	o := AgentOptions{
		Runner:       []string{stub, "--bare", "-p"},
		Prompt:       "check {{.name}}",
		Env:          map[string]any{"name": "item"},
		MCP:          map[string]MCPServer{"read": {URL: "https://example.test/mcp", Headers: map[string]string{"Authorization": "Bearer secret"}}, "local": {Command: []string{"server", "--read"}}},
		AllowedTools: []string{"mcp__read__get", "Read"},
		MaxTurns:     3,
		MaxBudgetUSD: 1.25,
		Sandbox:      SandboxOptions{Mode: "none"},
		WorkDir:      dir,
	}
	// agentArgv writes the scoped MCP config; check it before RunAgent removes it.
	path := filepath.Join(dir, "mcp.json")
	if _, _, err := agentArgv(o); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config mode=%v", info.Mode())
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("work dir mode=%v", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{"mcpServers": map[string]any{
		"read":  map[string]any{"type": "http", "url": "https://example.test/mcp", "headers": map[string]any{"Authorization": "Bearer secret"}},
		"local": map[string]any{"command": "server", "args": []any{"--read"}},
	}}
	if !reflect.DeepEqual(config, expected) {
		t.Fatalf("config=%#v", config)
	}
	got, err := RunAgent(context.Background(), o)
	if err != nil || got.Exit != 0 {
		t.Fatalf("exit=%d output=%q err=%v", got.Exit, got.Output, err)
	}
	want := []string{"--bare", "-p", "check item", "--strict-mcp-config", "--mcp-config", path, "--tools", "", "--allowedTools", "mcp__read__get,Read", "--permission-mode", "dontAsk", "--max-turns", "3", "--max-budget-usd", "1.25", "--output-format", "json"}
	if !reflect.DeepEqual(got.JSON, anySlice(want)) {
		t.Fatalf("argv=%#v want=%#v", got.JSON, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mcp.json must be removed after the run (holds bearer headers), stat err=%v", err)
	}

	o.Runner = []string{stub, "--echo-config"}
	o.Secrets = []string{"secret"}
	got, err = RunAgent(context.Background(), o)
	if err != nil || got.Exit != 0 || strings.Contains(string(got.Output), "secret") || !strings.Contains(string(got.Output), "Bearer ***") {
		t.Fatalf("masked output=%q err=%v", got.Output, err)
	}
	if got.JSON != nil {
		t.Fatalf("mixed stdout/stderr parsed as JSON: %#v", got.JSON)
	}
}

func TestAgentArgv(t *testing.T) {
	dir := t.TempDir()
	o := AgentOptions{Prompt: "-leading {{.value}}", Env: map[string]any{"value": "ok"}, WorkDir: dir, Timeout: time.Minute}
	argv, sandbox, err := agentArgv(o)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^agentgw-agent-[0-9a-f]{16}\.service$`).MatchString(sandbox.Unit) {
		t.Fatalf("unit=%q", sandbox.Unit)
	}
	credential := "/run/credentials/" + sandbox.Unit + "/mcp.json"
	want := []string{"claude", "--bare", "-p", "-leading ok", "--strict-mcp-config", "--mcp-config", credential, "--tools", "", "--permission-mode", "dontAsk", "--output-format", "json"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv=%q want=%q", argv, want)
	}
	full := SandboxArgv(argv, sandbox.Timeout, sandbox.Credentials, sandbox.Unit)
	if !contains(full, "--unit="+sandbox.Unit) || !contains(full, "--property=LoadCredential=mcp.json:"+filepath.Join(dir, "mcp.json")) {
		t.Fatalf("sandbox argv=%q", full)
	}
	o.Runner = []string{"{{.runner}}"}
	if _, _, err := agentArgv(o); err == nil {
		t.Fatal("templated runner accepted")
	}
	o.Runner = nil
	o.Prompt = "{{.missing}}"
	if _, _, err := agentArgv(o); err == nil {
		t.Fatal("missing prompt key accepted")
	}
}

func anySlice(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Step 11: --bare reads only ANTHROPIC_API_KEY or apiKeyHelper, so the key file
// reaches the sandbox as a credential and apiKeyHelper points at it.
func TestAgentAPIKeyCredential(t *testing.T) {
	o := AgentOptions{Prompt: "p", WorkDir: t.TempDir(), APIKeyFile: "/run/agenix/anthropic", Sandbox: SandboxOptions{Mode: "systemd"}}
	argv, sb, err := agentArgv(o)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Credentials["api-key"] != "/run/agenix/anthropic" {
		t.Fatalf("credentials=%v", sb.Credentials)
	}
	want := `{"apiKeyHelper":"cat /run/credentials/` + sb.Unit + `/api-key"}`
	for i, a := range argv {
		if a == "--settings" && argv[i+1] == want {
			return
		}
	}
	t.Fatalf("argv lacks --settings %s: %q", want, argv)
}

func TestAgentAPIKeyPathRejected(t *testing.T) {
	o := AgentOptions{Prompt: "p", WorkDir: t.TempDir(), APIKeyFile: "/tmp/x;rm -rf ~", Sandbox: SandboxOptions{Mode: "none"}}
	if _, _, err := agentArgv(o); err == nil {
		t.Fatal("unsafe api key path accepted")
	}
}
