package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

var safePath = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

type MCPServer struct {
	URL     string
	Command []string
	Headers map[string]string
}

type AgentOptions struct {
	Kind         string
	Command      string
	Runner       []string // deprecated: only Runner[0] is used for Claude
	CredFiles    map[string][]byte
	APIKey       string
	APIKeyFile   string
	Prompt       string
	Env          map[string]any
	MCP          map[string]MCPServer
	AllowedTools []string
	MaxTurns     int
	MaxBudgetUSD float64
	Timeout      time.Duration
	Sandbox      SandboxOptions
	Secrets      []string
	WorkDir      string
}

type AgentResult struct {
	Exit      int
	Output    []byte
	Stdout    []byte
	Result    string
	Raw       any
	Class     string
	Writeback map[string][]byte
}

func RunAgent(ctx context.Context, o AgentOptions) (AgentResult, error) {
	if o.WorkDir != "" {
		defer os.RemoveAll(o.WorkDir)
	}
	argv, stdin, env, files, writeback, wbStore, err := buildRun(o)
	if err != nil {
		return AgentResult{Exit: -1}, err
	}
	if (o.Kind == "" || o.Kind == "claude") && o.Sandbox.Mode == "none" {
		path := filepath.Join(o.WorkDir, "mcp.json")
		if err := os.WriteFile(path, files["mcp.json"], 0o600); err != nil {
			return AgentResult{Exit: -1}, err
		}
		for i := range argv {
			if argv[i] == "--mcp-config" {
				argv[i+1] = path
				break
			}
		}
	}
	if o.Kind == "codex" && o.Sandbox.Mode == "none" {
		home := filepath.Join(o.WorkDir, ".codex")
		if err := os.MkdirAll(home, 0o700); err != nil {
			return AgentResult{Exit: -1}, err
		}
		if auth, ok := files[".codex/auth.json"]; ok {
			if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0o600); err != nil {
				return AgentResult{Exit: -1}, err
			}
		}
		env["CODEX_HOME"] = home
	}
	sb := o.Sandbox
	sb.Timeout = o.Timeout
	sb.Env = make(map[string]string, len(o.Sandbox.Env)+len(env))
	for k, v := range o.Sandbox.Env {
		sb.Env[k] = v
	}
	for k, v := range env {
		sb.Env[k] = v
	}
	sb.Files = make(map[string][]byte, len(o.Sandbox.Files)+len(files))
	for k, v := range o.Sandbox.Files {
		sb.Files[k] = v
	}
	for k, v := range files {
		sb.Files[k] = v
	}
	sb.Writeback = append(append([]string(nil), o.Sandbox.Writeback...), writeback...)
	secrets := append([]string(nil), o.Secrets...)
	secrets = append(secrets, o.APIKey)
	for _, b := range o.CredFiles {
		secrets = append(secrets, string(b))
		tokenStrings(b, &secrets)
	}
	if o.APIKeyFile != "" {
		secrets = append(secrets, string(files["api-key"]))
	}
	exit, output, stdout, wb, err := runCommand(ctx, argv, sb, secrets, stdin, true)
	if o.Kind == "codex" && o.Sandbox.Mode == "none" && len(writeback) != 0 {
		if changed := readCapped(filepath.Join(o.WorkDir, ".codex", "auth.json")); changed != nil && !bytes.Equal(changed, files[".codex/auth.json"]) {
			if wb == nil {
				wb = map[int][]byte{}
			}
			wb[len(o.Sandbox.Writeback)] = changed
		}
	}
	result, raw := parseResult(o.Kind, stdout)
	out := AgentResult{Exit: exit, Output: output, Stdout: stdout, Result: result, Raw: raw, Class: classify(o.Kind, output)}
	if len(wb) > 0 {
		out.Writeback = map[string][]byte{}
		for i, b := range wb {
			if i >= len(o.Sandbox.Writeback) && i-len(o.Sandbox.Writeback) < len(wbStore) {
				out.Writeback[wbStore[i-len(o.Sandbox.Writeback)]] = b
			}
		}
	}
	return out, err
}

func buildRun(o AgentOptions) (argv []string, stdin []byte, env map[string]string, files map[string][]byte, writeback []string, wbStore []string, err error) {
	kind := o.Kind
	if kind == "" {
		kind = "claude"
	}
	cmd := o.Command
	if cmd == "" && kind == "claude" && len(o.Runner) > 0 {
		cmd = o.Runner[0]
	}
	if cmd == "" {
		cmd = kind
	}
	if strings.Contains(cmd, "{{") {
		err = errors.New("runner name must not contain a template action")
		return
	}
	t, e := template.New("prompt").Option("missingkey=error").Parse(o.Prompt)
	if e != nil {
		err = e
		return
	}
	var rendered bytes.Buffer
	if err = t.Execute(&rendered, o.Env); err != nil {
		return
	}
	prompt := rendered.String()
	if o.WorkDir == "" {
		err = errors.New("empty agent work directory")
		return
	}
	if err = os.MkdirAll(o.WorkDir, 0700); err != nil {
		return
	}
	if err = os.Chmod(o.WorkDir, 0700); err != nil {
		return
	}
	switch kind {
	case "claude":
		return buildClaude(o, cmd, prompt)
	case "codex":
		return buildCodex(o, cmd, prompt)
	case "agy":
		return buildAgy(o, cmd, prompt)
	default:
		err = fmt.Errorf("unknown agent kind %q", kind)
		return
	}
}

func parseResult(kind string, stdout []byte) (string, any) {
	if kind == "" {
		kind = "claude"
	}
	if kind == "codex" {
		return strings.TrimSpace(string(stdout)), nil
	}
	var raw map[string]any
	if json.Unmarshal(stdout, &raw) != nil {
		return "", nil
	}
	field := "result"
	if kind == "agy" {
		field = "response"
	}
	result, _ := raw[field].(string)
	return result, raw
}

func classify(kind string, output []byte) string {
	s := strings.ToLower(string(output))
	for _, v := range []string{"resource_exhausted", "quota", "rate limit", "429", "usage limit"} {
		if strings.Contains(s, v) {
			return "quota"
		}
	}
	for _, v := range []string{"invalid_grant", "not logged in", "unauthorized", "401", "please log in", "login required", "refresh token", "authentication"} {
		if strings.Contains(s, v) {
			return "auth"
		}
	}
	return ""
}

func tokenStrings(b []byte, secrets *[]string) {
	var value any
	if json.Unmarshal(b, &value) != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "access_token" || k == "refresh_token" || k == "accessToken" || k == "refreshToken" {
					if s, ok := v.(string); ok {
						*secrets = append(*secrets, s)
					}
				}
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(value)
}

func mcpConfig(mcp map[string]MCPServer, agy bool) ([]byte, error) {
	servers := make(map[string]any, len(mcp))
	for n, s := range mcp {
		switch {
		case s.URL != "" && len(s.Command) == 0:
			if agy {
				headers := s.Headers
				if headers == nil {
					headers = map[string]string{}
				}
				servers[n] = map[string]any{"serverUrl": s.URL, "headers": headers}
			} else {
				h := s.Headers
				if h == nil {
					h = map[string]string{}
				}
				servers[n] = map[string]any{"type": "http", "url": s.URL, "headers": h}
			}
		case s.URL == "" && len(s.Command) > 0:
			servers[n] = map[string]any{"command": s.Command[0], "args": append([]string{}, s.Command[1:]...)}
		default:
			return nil, fmt.Errorf("mcp server %q needs one URL or command", n)
		}
	}
	return json.Marshal(map[string]any{"mcpServers": servers})
}
