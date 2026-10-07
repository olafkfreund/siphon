package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"
)

var authError = regexp.MustCompile(`(?i)\b(?:401|invalid_grant|not logged in|authentication_error|unauthori[sz]ed)\b`)
var quotaError = regexp.MustCompile(`(?i)\b(?:429|RESOURCE_EXHAUSTED|quota|rate.?limit)\b`)

type MCPServer struct {
	URL     string
	Command []string
	Headers map[string]string
	// Env (stdio only) holds secret values: the server runs in its own bridge
	// unit and the agent gets a loopback URL instead. Egress is that unit's allowlist.
	Env    map[string]string
	Egress *EgressEnv
}

type AgentOptions struct {
	Kind         string
	Model        string // kind model: model id
	BaseURL      string // kind model: OpenAI-compatible base URL
	Command      string
	Runner       []string // deprecated: only Runner[0] is used for Claude
	CredFiles    map[string][]byte
	APIKey       string
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
	StateDir     string // directory of server.db: bridge secrets go in <StateDir>/bridge-secrets
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
	forwards, direct, stopBridges, err := startBridges(ctx, &o)
	if err != nil {
		return AgentResult{Exit: -1}, err
	}
	defer stopBridges()
	home := jobFilesDir
	if o.Sandbox.Mode == "none" {
		home = o.WorkDir
	}
	argv, stdin, env, files, writeback, wbStore, err := buildRun(o, home)
	if err != nil {
		return AgentResult{Exit: -1}, err
	}
	sb := o.Sandbox
	if sb.Mode == "none" {
		sb.home = home
	}
	var stderr []byte
	sb.stderr = &stderr
	sb.Timeout = o.Timeout
	sb.Env = make(map[string]string, len(o.Sandbox.Env)+len(env))
	for k, v := range o.Sandbox.Env {
		sb.Env[k] = v
	}
	for k, v := range env {
		sb.Env[k] = v
	}
	if sb.Egress != nil && kindOrDefault(o.Kind) != "model" {
		sb.Env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	}
	sb.Files = make(map[string][]byte, len(o.Sandbox.Files)+len(files))
	for k, v := range o.Sandbox.Files {
		sb.Files[k] = v
	}
	for k, v := range files {
		sb.Files[k] = v
	}
	sb.Forwards, sb.Direct = forwards, direct
	sb.Writeback = append(append([]string(nil), o.Sandbox.Writeback...), writeback...)
	secrets := append([]string(nil), o.Secrets...)
	secrets = append(secrets, o.APIKey)
	for _, b := range o.CredFiles {
		secrets = append(secrets, string(b))
		tokenStrings(b, &secrets)
	}
	exit, output, stdout, wb, err := runCommand(ctx, argv, sb, secrets, stdin, true)
	result, raw := parseResult(o.Kind, stdout)
	if o.Kind == "agy" {
		if m, ok := raw.(map[string]any); ok && m["status"] == "ERROR" && exit == 0 {
			exit = 1
		}
	}
	out := AgentResult{Exit: exit, Output: output, Stdout: stdout, Result: result, Raw: raw}
	if exit != 0 {
		out.Class = classify(o.Kind, raw, stderr)
	}
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

func kindOrDefault(k string) string {
	if k == "" {
		return "claude"
	}
	return k
}

func buildRun(o AgentOptions, home string) (argv []string, stdin []byte, env map[string]string, files map[string][]byte, writeback []string, wbStore []string, err error) {
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
	prompt, err := RenderPrompt(o.Prompt, o.Env)
	if err != nil {
		return
	}
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
		return buildClaude(o, cmd, prompt, home)
	case "codex":
		return buildCodex(o, cmd, prompt, home)
	case "agy":
		return buildAgy(o, cmd, prompt)
	case "model":
		return buildModel(o, prompt, home)
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
	value, err := decodeJSON(stdout)
	if err != nil {
		return "", nil
	}
	raw, _ = value.(map[string]any)
	if raw == nil {
		return "", nil
	}
	field := "result"
	if kind == "agy" {
		field = "response"
	}
	result, _ := raw[field].(string)
	return result, raw
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return convertNumbers(value)
}

func convertNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		return v.Float64()
	case map[string]any:
		for key, item := range v {
			converted, err := convertNumbers(item)
			if err != nil {
				return nil, err
			}
			v[key] = converted
		}
	case []any:
		for i, item := range v {
			converted, err := convertNumbers(item)
			if err != nil {
				return nil, err
			}
			v[i] = converted
		}
	}
	return value, nil
}

func classify(kind string, raw any, stderr []byte) string {
	var structured string
	if m, ok := raw.(map[string]any); ok {
		switch kind {
		case "", "claude":
			if m["is_error"] == true {
				structured, _ = m["result"].(string)
			}
		case "agy":
			structured, _ = m["error"].(string)
		}
	}
	if authError.MatchString(structured) {
		return "auth"
	}
	if quotaError.MatchString(structured) {
		return "quota"
	}
	if kind == "agy" {
		return ""
	}
	if quotaError.Match(stderr) {
		return "quota"
	}
	if authError.Match(stderr) {
		return "auth"
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
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		case string:
			if len(x) >= 20 {
				*secrets = append(*secrets, x)
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

// RenderPrompt renders an agent prompt template against a job's env, the
// same way a run does (missing keys are an error). The portal uses it to show
// what an agent will be asked.
func RenderPrompt(prompt string, env any) (string, error) {
	t, err := template.New("prompt").Option("missingkey=error").Parse(prompt)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, env); err != nil {
		return "", err
	}
	return b.String(), nil
}
