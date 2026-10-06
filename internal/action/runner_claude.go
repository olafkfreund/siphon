package action

import (
	"path/filepath"
	"strconv"
	"strings"
)

func buildClaude(o AgentOptions, cmd, prompt, home string) ([]string, []byte, map[string]string, map[string][]byte, []string, []string, error) {
	config, err := mcpConfig(o.MCP, false)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	env := map[string]string{}
	files := map[string][]byte{"mcp.json": config}
	argv := []string{cmd, "-p"}
	if o.APIKey != "" || len(o.CredFiles) == 0 {
		argv = append(argv, "--bare")
	}
	if o.APIKey != "" {
		env["ANTHROPIC_API_KEY"] = o.APIKey
	}
	var wb, store []string
	if o.APIKey == "" {
		if b, ok := o.CredFiles["credentials.json"]; ok {
			files[".claude/.credentials.json"] = b
			wb = append(wb, ".claude/.credentials.json")
			store = append(store, "credentials.json")
		}
		if b, ok := o.CredFiles["oauth-token"]; ok {
			env["CLAUDE_CODE_OAUTH_TOKEN"] = string(b)
		}
	}
	argv = append(argv, "--strict-mcp-config", "--mcp-config", filepath.Join(home, "mcp.json"), "--tools", "")
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
	return argv, []byte(prompt), env, files, wb, store, nil
}
