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
	"strconv"
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
	Runner       []string
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
	APIKeyFile   string // optional; exposed to the runner via --settings apiKeyHelper
}

type AgentResult struct {
	Exit   int
	Output []byte
	Stdout []byte
}

func RunAgent(ctx context.Context, o AgentOptions) (AgentResult, error) {
	defer os.RemoveAll(o.WorkDir)
	argv, prompt, sandbox, err := agentArgv(o)
	if err != nil {
		return AgentResult{Exit: -1}, err
	}
	exit, output, stdout, err := runCommand(ctx, argv, sandbox, o.Secrets, prompt, true)
	return AgentResult{Exit: exit, Output: output, Stdout: stdout}, err
}

func agentArgv(o AgentOptions) ([]string, []byte, SandboxOptions, error) {
	runner := o.Runner
	if len(runner) == 0 {
		runner = []string{"claude", "--bare", "-p"}
	}
	if strings.Contains(runner[0], "{{") {
		return nil, nil, o.Sandbox, errors.New("runner name must not contain a template action")
	}
	t, err := template.New("prompt").Option("missingkey=error").Parse(o.Prompt)
	if err != nil {
		return nil, nil, o.Sandbox, err
	}
	var prompt bytes.Buffer
	if err := t.Execute(&prompt, o.Env); err != nil {
		return nil, nil, o.Sandbox, err
	}
	if o.WorkDir == "" {
		return nil, nil, o.Sandbox, errors.New("empty agent work directory")
	}
	if err := os.MkdirAll(o.WorkDir, 0700); err != nil {
		return nil, nil, o.Sandbox, err
	}
	if err := os.Chmod(o.WorkDir, 0700); err != nil {
		return nil, nil, o.Sandbox, err
	}
	servers := make(map[string]any, len(o.MCP))
	for name, server := range o.MCP {
		switch {
		case server.URL != "" && len(server.Command) == 0:
			headers := server.Headers
			if headers == nil {
				headers = map[string]string{}
			}
			servers[name] = map[string]any{"type": "http", "url": server.URL, "headers": headers}
		case server.URL == "" && len(server.Command) > 0:
			servers[name] = map[string]any{"command": server.Command[0], "args": append([]string{}, server.Command[1:]...)}
		default:
			return nil, nil, o.Sandbox, fmt.Errorf("mcp server %q needs one URL or command", name)
		}
	}
	config, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil, nil, o.Sandbox, err
	}
	sandbox := o.Sandbox
	sandbox.Timeout = o.Timeout
	keyPath := o.APIKeyFile
	if keyPath != "" && !safePath.MatchString(keyPath) {
		return nil, nil, sandbox, fmt.Errorf("api key file %q: only [A-Za-z0-9/._-] allowed", keyPath)
	}
	var path string
	if sandbox.Mode == "" || sandbox.Mode == "systemd" {
		// The MCP config (bearer headers) and API key travel inside the job spec
		// and land only in the unit's private /tmp, never in a readable path.
		sandbox.Files = map[string][]byte{"mcp.json": config}
		for k, v := range o.Sandbox.Files {
			sandbox.Files[k] = v
		}
		path = FilePath("mcp.json")
		if keyPath != "" {
			key, err := os.ReadFile(keyPath)
			if err != nil {
				return nil, nil, sandbox, fmt.Errorf("api key file: %w", err)
			}
			sandbox.Files["api-key"] = key
			keyPath = FilePath("api-key")
		}
	} else {
		path = filepath.Join(o.WorkDir, "mcp.json")
		if err := os.WriteFile(path, config, 0o600); err != nil {
			return nil, nil, sandbox, err
		}
	}
	argv := append([]string(nil), runner...)
	argv = append(argv, "--strict-mcp-config", "--mcp-config", path, "--tools", "")
	if len(o.AllowedTools) > 0 {
		argv = append(argv, "--allowedTools", strings.Join(o.AllowedTools, ","))
	}
	argv = append(argv, "--permission-mode", "dontAsk")
	if o.MaxTurns != 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(o.MaxTurns))
	}
	if o.MaxBudgetUSD != 0 {
		argv = append(argv, "--max-budget-usd", strconv.FormatFloat(o.MaxBudgetUSD, 'f', -1, 64))
	}
	if keyPath != "" {
		// apiKeyHelper runs through a shell; keyPath is config-only and safePath-checked.
		settings, _ := json.Marshal(map[string]string{"apiKeyHelper": "cat " + keyPath})
		argv = append(argv, "--settings", string(settings))
	}
	argv = append(argv, "--output-format", "json")
	return argv, prompt.Bytes(), sandbox, nil
}
