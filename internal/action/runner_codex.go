package action

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var tomlName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlString(s string) string { b, _ := json.Marshal(s); return string(b) }
func tomlArray(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

func buildCodex(o AgentOptions, cmd, prompt string) ([]string, []byte, map[string]string, map[string][]byte, []string, []string, error) {
	method := "chatgpt"
	files := map[string][]byte{}
	var wb, store []string
	if o.APIKey != "" {
		method = "api"
		files[".codex/auth.json"], _ = json.Marshal(map[string]string{"OPENAI_API_KEY": o.APIKey})
	} else if b, ok := o.CredFiles["auth.json"]; ok {
		files[".codex/auth.json"] = b
		wb = []string{".codex/auth.json"}
		store = []string{"auth.json"}
	}
	argv := []string{cmd, "exec", "--skip-git-repo-check", "--ephemeral", "--strict-config", "-s", "read-only", "-c", "forced_login_method=" + tomlString(method)}
	names := make([]string, 0, len(o.MCP))
	for n := range o.MCP {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !tomlName.MatchString(n) {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("unsafe mcp server name %q", n)
		}
		s := o.MCP[n]
		prefix := "mcp_servers." + n + "."
		switch {
		case s.URL != "" && len(s.Command) == 0:
			argv = append(argv, "-c", prefix+"url="+tomlString(s.URL))
			if len(s.Headers) > 0 {
				keys := make([]string, 0, len(s.Headers))
				for k := range s.Headers {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				pairs := make([]string, 0, len(keys))
				for _, k := range keys {
					pairs = append(pairs, tomlString(k)+"="+tomlString(s.Headers[k]))
				}
				argv = append(argv, "-c", prefix+"http_headers={"+strings.Join(pairs, ",")+"}")
			}
		case s.URL == "" && len(s.Command) > 0:
			argv = append(argv, "-c", prefix+"command="+tomlString(s.Command[0]), "-c", prefix+"args="+tomlArray(s.Command[1:]))
		default:
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("mcp server %q needs one URL or command", n)
		}
		tools := []string{}
		for _, allowed := range o.AllowedTools {
			if tool, ok := strings.CutPrefix(allowed, "mcp__"+n+"__"); ok && tool != "" {
				if !tomlName.MatchString(tool) {
					return nil, nil, nil, nil, nil, nil, fmt.Errorf("unsafe mcp tool name %q", tool)
				}
				tools = append(tools, tool)
			}
		}
		sort.Strings(tools)
		argv = append(argv, "-c", prefix+"enabled_tools="+tomlArray(tools))
		for _, tool := range tools {
			argv = append(argv, "-c", prefix+"tools."+tool+".approval_mode="+tomlString("approve"))
		}
	}
	argv = append(argv, "-")
	return argv, []byte(prompt), map[string]string{"CODEX_HOME": FilePath(".codex")}, files, wb, store, nil
}
