package action

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
)

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
}

type AgentResult struct {
	Exit   int
	Output []byte
	JSON   any
}

func RunAgent(ctx context.Context, o AgentOptions) (AgentResult, error) {
	argv, sandbox, err := agentArgv(o)
	if err != nil {
		return AgentResult{Exit: -1}, err
	}
	exit, output, err := runCommand(ctx, argv, sandbox, o.Secrets)
	result := AgentResult{Exit: exit, Output: output}
	dec := json.NewDecoder(bytes.NewReader(output))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) == nil {
		var extra any
		if dec.Decode(&extra) == io.EOF {
			result.JSON = value
		}
	}
	return result, err
}

func agentArgv(o AgentOptions) ([]string, SandboxOptions, error) {
	runner := o.Runner
	if len(runner) == 0 {
		runner = []string{"claude", "--bare", "-p"}
	}
	if strings.Contains(runner[0], "{{") {
		return nil, o.Sandbox, errors.New("runner name must not contain a template action")
	}
	t, err := template.New("prompt").Option("missingkey=error").Parse(o.Prompt)
	if err != nil {
		return nil, o.Sandbox, err
	}
	var prompt bytes.Buffer
	if err := t.Execute(&prompt, o.Env); err != nil {
		return nil, o.Sandbox, err
	}
	if o.WorkDir == "" {
		return nil, o.Sandbox, errors.New("empty agent work directory")
	}
	if err := os.MkdirAll(o.WorkDir, 0700); err != nil {
		return nil, o.Sandbox, err
	}
	if err := os.Chmod(o.WorkDir, 0700); err != nil {
		return nil, o.Sandbox, err
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
			return nil, o.Sandbox, fmt.Errorf("mcp server %q needs one URL or command", name)
		}
	}
	config, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil, o.Sandbox, err
	}
	file, err := os.CreateTemp(o.WorkDir, ".mcp-*")
	if err != nil {
		return nil, o.Sandbox, err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, o.Sandbox, err
	}
	if _, err := file.Write(config); err != nil {
		file.Close()
		return nil, o.Sandbox, err
	}
	if err := file.Close(); err != nil {
		return nil, o.Sandbox, err
	}
	path := filepath.Join(o.WorkDir, "mcp.json")
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, o.Sandbox, err
	}
	sandbox := o.Sandbox
	sandbox.Timeout = o.Timeout
	if sandbox.Mode == "" || sandbox.Mode == "systemd" {
		id := make([]byte, 8)
		if _, err := rand.Read(id); err != nil {
			return nil, sandbox, err
		}
		sandbox.Unit = "agentgw-agent-" + hex.EncodeToString(id) + ".service"
		sandbox.Credentials = make(map[string]string, len(o.Sandbox.Credentials)+1)
		for name, value := range o.Sandbox.Credentials {
			sandbox.Credentials[name] = value
		}
		sandbox.Credentials["mcp.json"] = path
		path = "/run/credentials/" + sandbox.Unit + "/mcp.json"
	}
	argv := append([]string(nil), runner...)
	argv = append(argv, prompt.String(), "--strict-mcp-config", "--mcp-config", path, "--tools", "")
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
	argv = append(argv, "--output-format", "json")
	return argv, sandbox, nil
}
