package action

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	if _, _, _, err := agentArgv(o); err != nil {
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
	want := []string{"--bare", "-p", "--strict-mcp-config", "--mcp-config", path, "--tools", "", "--allowedTools", "mcp__read__get,Read", "--permission-mode", "dontAsk", "--max-turns", "3", "--max-budget-usd", "1.25", "--output-format", "json"}
	var response struct {
		Argv  []string `json:"argv"`
		Stdin string   `json:"stdin"`
	}
	if err := json.Unmarshal(got.Stdout, &response); err != nil || !reflect.DeepEqual(response.Argv, want) || response.Stdin != "check item" {
		t.Fatalf("stdout=%q want argv=%q stdin=%q err=%v", got.Stdout, want, "check item", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mcp.json must be removed after the run (holds bearer headers), stat err=%v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("work dir must be removed after the run, stat err=%v", err)
	}

	o.Runner = []string{stub, "--echo-config"}
	o.Prompt = "secret"
	o.Secrets = []string{"secret"}
	got, err = RunAgent(context.Background(), o)
	if err != nil || got.Exit != 0 || strings.Contains(string(got.Output), "secret") || !strings.Contains(string(got.Output), "Bearer ***") {
		t.Fatalf("masked output=%q err=%v", got.Output, err)
	}
	if strings.Contains(string(got.Stdout), "secret") || !strings.Contains(string(got.Stdout), `"stdin":"***"`) || !json.Valid(got.Stdout) {
		t.Fatalf("stdout must contain only runner JSON: %q", got.Stdout)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("work dir must be removed after the run, stat err=%v", err)
	}

	o.Runner = []string{stub, "--fail"}
	got, err = RunAgent(context.Background(), o)
	if err != nil || got.Exit != 7 || !json.Valid(got.Stdout) {
		t.Fatalf("failed runner: exit=%d stdout=%q err=%v", got.Exit, got.Stdout, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("work dir must be removed after failure, stat err=%v", err)
	}

	o.Runner = []string{stub, "--large"}
	got, err = RunAgent(context.Background(), o)
	if err != nil || got.Exit != 0 || len(got.Stdout) != 1<<20 || len(got.Output) != 64<<10 {
		t.Fatalf("caps: exit=%d stdout=%d output=%d err=%v", got.Exit, len(got.Stdout), len(got.Output), err)
	}
}

func TestAgentArgv(t *testing.T) {
	dir := t.TempDir()
	o := AgentOptions{Prompt: "-leading {{.value}}", Env: map[string]any{"value": "ok"}, WorkDir: dir, Timeout: time.Minute}
	argv, prompt, sandbox, err := agentArgv(o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--bare", "-p", "--strict-mcp-config", "--mcp-config", FilePath("mcp.json"), "--tools", "", "--permission-mode", "dontAsk", "--output-format", "json"}
	if !reflect.DeepEqual(argv, want) || string(prompt) != "-leading ok" {
		t.Fatalf("argv=%q want=%q", argv, want)
	}
	// systemd mode: the config travels in the job spec, never as a readable file.
	if !strings.Contains(string(sandbox.Files["mcp.json"]), "mcpServers") {
		t.Fatalf("job files=%v", sandbox.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("mcp.json written to the work dir in systemd mode")
	}
	o.Runner = []string{"{{.runner}}"}
	if _, _, _, err := agentArgv(o); err == nil {
		t.Fatal("templated runner accepted")
	}
	o.Runner = nil
	o.Prompt = "{{.missing}}"
	if _, _, _, err := agentArgv(o); err == nil {
		t.Fatal("missing prompt key accepted")
	}
	o.Prompt = "{{.value}}"
	o.Env = map[string]any{"value": `--mcp-config={"mcpServers":{}}`}
	argv, prompt, _, err = agentArgv(o)
	if err != nil || string(prompt) != `--mcp-config={"mcpServers":{}}` {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
	for _, arg := range argv {
		if strings.Contains(arg, string(prompt)) {
			t.Fatalf("rendered prompt in argv: %q", argv)
		}
	}
}

func TestRunAgentWriteback(t *testing.T) {
	o := AgentOptions{
		Runner: []string{"sh", "-c", `printf new > "$HOME/.codex/auth.json"; printf '{"result":"ok"}'`},
		Prompt: "p", WorkDir: filepath.Join(t.TempDir(), "work"),
		Sandbox: SandboxOptions{Mode: "none", Files: map[string][]byte{".codex/auth.json": []byte("old")}, Writeback: []string{".codex/auth.json"}},
	}
	got, err := RunAgent(context.Background(), o)
	if err != nil || got.Exit != 0 || string(got.Writeback[0]) != "new" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Step 11: --bare reads only ANTHROPIC_API_KEY or apiKeyHelper. In systemd
// mode the key travels in the job spec and apiKeyHelper reads the unit-private copy.
func TestAgentAPIKeyInJobFiles(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "anthropic")
	if err := os.WriteFile(keyFile, []byte("sk-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := AgentOptions{Prompt: "p", WorkDir: t.TempDir(), APIKeyFile: keyFile, Sandbox: SandboxOptions{Mode: "systemd"}}
	argv, _, sb, err := agentArgv(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(sb.Files["api-key"]) != "sk-test" {
		t.Fatalf("job files=%v", sb.Files)
	}
	want := `{"apiKeyHelper":"cat ` + FilePath("api-key") + `"}`
	for i, a := range argv {
		if a == "--settings" && argv[i+1] == want {
			return
		}
	}
	t.Fatalf("argv lacks --settings %s: %q", want, argv)
}

func TestAgentAPIKeyPathRejected(t *testing.T) {
	o := AgentOptions{Prompt: "p", WorkDir: t.TempDir(), APIKeyFile: "/tmp/x;rm -rf ~", Sandbox: SandboxOptions{Mode: "none"}}
	if _, _, _, err := agentArgv(o); err == nil {
		t.Fatal("unsafe api key path accepted")
	}
}
