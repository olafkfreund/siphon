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

func TestBuildRun(t *testing.T) {
	base := AgentOptions{Prompt: "hello {{.who}}", Env: map[string]any{"who": "world"}, WorkDir: t.TempDir(), Timeout: 10 * time.Minute, MCP: map[string]MCPServer{"demo": {Command: []string{"serve", "--quiet"}}}, AllowedTools: []string{"mcp__demo__get_status"}}
	tests := []struct {
		name  string
		o     AgentOptions
		argv  []string
		stdin string
		env   map[string]string
		file  string
		wb    []string
		store []string
	}{
		{"claude subscription", func() AgentOptions {
			o := base
			o.Kind = "claude"
			o.CredFiles = map[string][]byte{"credentials.json": []byte("login")}
			return o
		}(), []string{"claude", "-p", "--strict-mcp-config", "--mcp-config", FilePath("mcp.json"), "--tools", "", "--allowedTools", "mcp__demo__get_status", "--permission-mode", "dontAsk", "--output-format", "json"}, "hello world", map[string]string{}, ".claude/.credentials.json", []string{".claude/.credentials.json"}, []string{"credentials.json"}},
		{"claude api", func() AgentOptions { o := base; o.APIKey = "key"; return o }(), []string{"claude", "-p", "--bare", "--strict-mcp-config", "--mcp-config", FilePath("mcp.json"), "--tools", "", "--allowedTools", "mcp__demo__get_status", "--permission-mode", "dontAsk", "--output-format", "json"}, "hello world", map[string]string{"ANTHROPIC_API_KEY": "key"}, "mcp.json", nil, nil},
		{"codex subscription", func() AgentOptions {
			o := base
			o.Kind = "codex"
			o.CredFiles = map[string][]byte{"auth.json": []byte("login")}
			return o
		}(), []string{"codex", "exec", "--skip-git-repo-check", "--ephemeral", "--strict-config", "-s", "read-only", "-c", `forced_login_method="chatgpt"`, "-c", `mcp_servers.demo.command="serve"`, "-c", `mcp_servers.demo.args=["--quiet"]`, "-c", `mcp_servers.demo.enabled_tools=["get_status"]`, "-c", `mcp_servers.demo.tools.get_status.approval_mode="approve"`, "-"}, "hello world", map[string]string{"CODEX_HOME": FilePath(".codex")}, ".codex/auth.json", []string{".codex/auth.json"}, []string{"auth.json"}},
		{"codex api", func() AgentOptions { o := base; o.Kind = "codex"; o.APIKey = "key"; o.MCP = nil; return o }(), []string{"codex", "exec", "--skip-git-repo-check", "--ephemeral", "--strict-config", "-s", "read-only", "-c", `forced_login_method="api"`, "-"}, "hello world", map[string]string{"CODEX_HOME": FilePath(".codex")}, ".codex/auth.json", nil, nil},
		{"agy subscription", func() AgentOptions {
			o := base
			o.Kind = "agy"
			o.CredFiles = map[string][]byte{"antigravity-oauth-token": []byte("login")}
			return o
		}(), []string{"agy", "--print=hello world", "--output-format", "json", "--mode", "plan", "--sandbox", "--print-timeout", "10m0s"}, "", map[string]string{}, ".gemini/antigravity-cli/antigravity-oauth-token", []string{".gemini/antigravity-cli/antigravity-oauth-token"}, []string{"antigravity-oauth-token"}},
		{"agy api", func() AgentOptions { o := base; o.Kind = "agy"; o.APIKey = "key"; return o }(), []string{"agy", "--print=hello world", "--output-format", "json", "--mode", "plan", "--sandbox", "--print-timeout", "10m0s"}, "", map[string]string{"GEMINI_API_KEY": "key"}, ".gemini/config/mcp_config.json", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv, stdin, env, files, wb, store, err := buildRun(tt.o)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(argv, tt.argv) || string(stdin) != tt.stdin || !reflect.DeepEqual(env, tt.env) || !reflect.DeepEqual(wb, tt.wb) || !reflect.DeepEqual(store, tt.store) {
				t.Fatalf("argv=%q\nstdin=%q env=%v wb=%v store=%v", argv, stdin, env, wb, store)
			}
			if _, ok := files[tt.file]; !ok {
				t.Fatalf("files=%v", files)
			}
		})
	}
}

func TestCodexTOMLAndAllowlist(t *testing.T) {
	o := AgentOptions{Kind: "codex", Prompt: "p", WorkDir: t.TempDir(), MCP: map[string]MCPServer{"demo": {URL: "https://test", Headers: map[string]string{"Authorization": `Bearer "quoted"`}}, "empty": {Command: []string{"server"}}}, AllowedTools: []string{"mcp__demo__get_status", "mcp__other__ignored"}}
	argv, _, _, _, _, _, err := buildRun(o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	for _, s := range []string{`mcp_servers.demo.http_headers={"Authorization"="Bearer \"quoted\""}`, `mcp_servers.demo.enabled_tools=["get_status"]`, `mcp_servers.demo.tools.get_status.approval_mode="approve"`, `mcp_servers.empty.enabled_tools=[]`} {
		if !strings.Contains(joined, s) {
			t.Fatalf("missing %q in %q", s, joined)
		}
	}
}

func TestAgyPromptOneArgument(t *testing.T) {
	o := AgentOptions{Kind: "agy", Prompt: `--mcp-config={}`, WorkDir: t.TempDir(), Timeout: time.Minute}
	argv, stdin, _, _, _, _, err := buildRun(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdin) != 0 || argv[1] != `--print=--mcp-config={}` {
		t.Fatalf("argv=%q stdin=%q", argv, stdin)
	}
}

func TestParseAndClassify(t *testing.T) {
	for _, tt := range []struct {
		kind, stdout, result string
		raw                  bool
	}{{"claude", `{"result":"done","cost":1}`, "done", true}, {"codex", " final answer \n", "final answer", false}, {"agy", `{"response":"okay","status":"SUCCESS"}`, "okay", true}} {
		result, raw := parseResult(tt.kind, []byte(tt.stdout))
		if result != tt.result || (raw != nil) != tt.raw {
			t.Fatalf("kind=%s result=%q raw=%v", tt.kind, result, raw)
		}
	}
	for _, tt := range []struct{ input, want string }{{"RESOURCE_EXHAUSTED (code 429): Individual quota reached", "quota"}, {"Status unavailable: tool approval required.", ""}, {`API Error: 401 {"type":"error","error":{"type":"authentication_error"}}`, "auth"}, {"invalid_grant: refresh token expired", "auth"}, {"quota authentication", "quota"}} {
		if got := classify("agy", []byte(tt.input)); got != tt.want {
			t.Fatalf("%q: %q", tt.input, got)
		}
	}
}

func TestClaudeLegacyRunnerAndKeyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	o := AgentOptions{Runner: []string{"custom", "--ignored"}, Prompt: "p", WorkDir: t.TempDir(), APIKeyFile: file}
	argv, _, _, files, _, _, err := buildRun(o)
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "custom" || argv[1] != "-p" || argv[2] != "--bare" || string(files["api-key"]) != "secret" {
		t.Fatalf("argv=%q files=%v", argv, files)
	}
	found := false
	for i, s := range argv {
		if s == "--settings" {
			var m map[string]string
			if json.Unmarshal([]byte(argv[i+1]), &m) != nil || m["apiKeyHelper"] != "cat "+FilePath("api-key") {
				t.Fatalf("settings=%q", argv[i+1])
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing settings")
	}
}

func TestPlacedFilesAndResults(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\ntest -f \"$HOME/.claude/.credentials.json\" || exit 2\ncat \"$HOME/.claude/.credentials.json\" >&2\nprintf '%s' '{\"result\":\"done\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := RunAgent(context.Background(), AgentOptions{Command: claude, Prompt: "p", WorkDir: filepath.Join(dir, "cw"), Sandbox: SandboxOptions{Mode: "none"}, CredFiles: map[string][]byte{"credentials.json": []byte(`{"accessToken":"secret-token"}`)}})
	if err != nil || c.Exit != 0 || c.Result != "done" || c.Raw == nil || strings.Contains(string(c.Output), "secret-token") {
		t.Fatalf("claude result=%+v err=%v", c, err)
	}
	agy := filepath.Join(dir, "agy")
	if err := os.WriteFile(agy, []byte("#!/bin/sh\ntest -f \"$HOME/.gemini/config/mcp_config.json\" || exit 2\ntest -f \"$HOME/.gemini/antigravity-cli/antigravity-oauth-token\" || exit 3\nprintf '%s' '{\"status\":\"ERROR\",\"error\":\"RESOURCE_EXHAUSTED (code 429)\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := RunAgent(context.Background(), AgentOptions{Kind: "agy", Command: agy, Prompt: "p", WorkDir: filepath.Join(dir, "aw"), Sandbox: SandboxOptions{Mode: "none"}, CredFiles: map[string][]byte{"antigravity-oauth-token": []byte("login")}})
	if err != nil || a.Exit != 0 || a.Class != "quota" || a.Raw == nil {
		t.Fatalf("agy result=%+v err=%v", a, err)
	}
}
