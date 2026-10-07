package action

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/olafkfreund/siphon/internal/agentloop"
)

// buildModel runs the built-in loop (`siphon agent-run`) as the agent command.
// The spec and the key sit in the run's private files, never in env or argv;
// os.Executable inside the unit is the same store path exec-job runs from.
func buildModel(o AgentOptions, prompt, home string) ([]string, []byte, map[string]string, map[string][]byte, []string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	spec := agentloop.Spec{
		BaseURL: o.BaseURL, Model: o.Model, Prompt: prompt, Allowed: o.AllowedTools,
		MaxTurns: o.MaxTurns, MaxCostUSD: o.MaxBudgetUSD, MCP: map[string]agentloop.MCPServer{},
	}
	for n, s := range o.MCP {
		spec.MCP[n] = agentloop.MCPServer{URL: s.URL, Command: s.Command, Headers: s.Headers}
	}
	files := map[string][]byte{}
	if o.APIKey != "" {
		files["model-key"] = []byte(o.APIKey)
		spec.KeyFile = filepath.Join(home, "model-key")
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	files["spec.json"] = b
	return []string{exe, "agent-run", filepath.Join(home, "spec.json")}, nil, map[string]string{}, files, nil, nil, nil
}
