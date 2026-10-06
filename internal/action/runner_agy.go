package action

import "time"

func buildAgy(o AgentOptions, cmd, prompt string) ([]string, []byte, map[string]string, map[string][]byte, []string, []string, error) {
	config, err := mcpConfig(o.MCP, true)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	files := map[string][]byte{".gemini/config/mcp_config.json": config}
	env := map[string]string{}
	var wb, store []string
	if o.APIKey != "" {
		env["GEMINI_API_KEY"] = o.APIKey
	} else if b, ok := o.CredFiles["antigravity-oauth-token"]; ok {
		files[".gemini/antigravity-cli/antigravity-oauth-token"] = b
		wb = []string{".gemini/antigravity-cli/antigravity-oauth-token"}
		store = []string{"antigravity-oauth-token"}
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	argv := []string{cmd, "--print=" + prompt, "--output-format", "json", "--mode", "plan", "--sandbox", "--print-timeout", timeout.String()}
	return argv, nil, env, files, wb, store, nil
}
