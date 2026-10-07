// Package agentloop is siphon's built-in agent for OpenAI-compatible
// endpoints (Ollama, LM Studio, OpenRouter, ...): a chat loop that exposes the
// agent's MCP tools to the model and runs the calls it makes.
package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxToolResult = 64 << 10
	maxTranscript = 512 << 10
	defaultTurns  = 20
	maxCalls      = 16 // tool calls the model may make in one turn
)

type MCPServer struct {
	URL     string            `json:"url,omitempty"`
	Command []string          `json:"command,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Spec struct {
	BaseURL    string               `json:"base_url"`
	Model      string               `json:"model"`
	KeyFile    string               `json:"key_file,omitempty"` // read here, never from env or argv
	Prompt     string               `json:"prompt"`
	MCP        map[string]MCPServer `json:"mcp,omitempty"`
	Allowed    []string             `json:"allowed,omitempty"` // mcp__<server>__<tool> (or mcp__<server>)
	MaxTurns   int                  `json:"max_turns,omitempty"`
	MaxCostUSD float64              `json:"max_cost_usd,omitempty"`
}

type call struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCalls  []call `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost,omitempty"`
}

type tool struct {
	sess *mcp.ClientSession
	name string
	def  map[string]any
}

// Run executes spec and prints one JSON result line to stdout. It returns the
// process exit code: 0 on an answer, 1 otherwise.
func Run(ctx context.Context, spec Spec, stdout io.Writer) int {
	return run(ctx, spec, newClient(), stdout)
}

func run(ctx context.Context, spec Spec, hc *http.Client, stdout io.Writer) int {
	key := ""
	if spec.KeyFile != "" {
		if b, err := os.ReadFile(spec.KeyFile); err == nil {
			key = strings.TrimSpace(string(b))
		}
	}
	l := &loop{spec: spec, hc: hc, key: key}
	res, turns, err := l.do(ctx)
	out := map[string]any{"type": "result", "result": res, "turns": turns, "usage": l.usage}
	if l.note != "" {
		out["note"] = l.note
	}
	code := 0
	if err != nil {
		out["is_error"], out["result"], code = true, l.scrub(err.Error()), 1
		fmt.Fprintln(os.Stderr, l.scrub(err.Error()))
	} else {
		out["result"] = l.scrub(res)
	}
	b, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", b)
	return code
}

type loop struct {
	spec  Spec
	hc    *http.Client
	key   string
	usage usage
	note  string
	msgs  []message
}

func (l *loop) scrub(s string) string {
	if l.key != "" {
		s = strings.ReplaceAll(s, l.key, "***")
	}
	return s
}

func (l *loop) do(ctx context.Context) (string, int, error) {
	if l.spec.BaseURL == "" || l.spec.Model == "" {
		return "", 0, errors.New("spec needs base_url and model")
	}
	tools, closeAll, err := l.connect(ctx)
	defer closeAll()
	if err != nil {
		return "", 0, err
	}
	var defs []map[string]any
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		defs = append(defs, map[string]any{"type": "function", "function": tools[n].def})
	}
	l.msgs = []message{{Role: "user", Content: l.spec.Prompt}}
	maxTurns := l.spec.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultTurns
	}
	for turn := 1; turn <= maxTurns; turn++ {
		m, err := l.chat(ctx, &defs)
		if err != nil {
			return "", turn, err
		}
		if l.spec.MaxCostUSD > 0 && l.usage.Cost > l.spec.MaxCostUSD {
			return "", turn, fmt.Errorf("max_budget_usd exceeded (%.4f)", l.usage.Cost)
		}
		if len(m.ToolCalls) == 0 {
			return m.Content, turn, nil
		}
		if len(m.ToolCalls) > maxCalls {
			return "", turn, fmt.Errorf("the model made %d tool calls in one turn (max %d)", len(m.ToolCalls), maxCalls)
		}
		l.msgs = append(l.msgs, m)
		for _, c := range m.ToolCalls {
			t := tools[c.Function.Name]
			var args map[string]any
			if t == nil || json.Unmarshal([]byte(c.Function.Arguments), &args) != nil {
				return "", turn, fmt.Errorf("malformed tool call %q", c.Function.Name)
			}
			l.msgs = append(l.msgs, message{Role: "tool", ToolCallID: c.ID, Content: callTool(ctx, t, args)})
			if err := l.trim(); err != nil {
				return "", turn, err
			}
		}
	}
	return "", maxTurns, fmt.Errorf("max_turns (%d) reached without a final answer", maxTurns)
}

// trim keeps the transcript under maxTranscript by emptying the oldest tool results first.
func (l *loop) trim() error {
	size := func() (n int) {
		for _, m := range l.msgs {
			n += len(m.Content)
			for _, c := range m.ToolCalls {
				n += len(c.Function.Arguments)
			}
		}
		return
	}
	for i := 0; size() > maxTranscript; i++ {
		for i < len(l.msgs) && (l.msgs[i].Role != "tool" || l.msgs[i].Content == "[trimmed]") {
			i++
		}
		if i == len(l.msgs) {
			return errors.New("conversation exceeds 512 KiB")
		}
		l.msgs[i].Content = "[trimmed]"
	}
	return nil
}

func callTool(ctx context.Context, t *tool, args map[string]any) string {
	res, err := t.sess.CallTool(ctx, &mcp.CallToolParams{Name: t.name, Arguments: args})
	if err != nil {
		return "error: " + err.Error()
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if sb.Len() == 0 && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		sb.Write(b)
	}
	s := sb.String()
	if res.IsError {
		s = "error: " + s
	}
	if len(s) > maxToolResult {
		s = s[:maxToolResult] + "\n[truncated]"
	}
	return s
}

// chat sends one completion. If the endpoint rejects `tools` with a 4xx it
// retries once without them and drops them for the rest of the run.
func (l *loop) chat(ctx context.Context, defs *[]map[string]any) (message, error) {
	m, status, err := l.post(ctx, *defs)
	if err != nil && len(*defs) > 0 && status >= 400 && status < 500 && status != 401 && status != 403 && status != 429 {
		*defs = nil
		l.note = "endpoint rejected tools; ran as a plain completion"
		m, _, err = l.post(ctx, nil)
	}
	return m, err
}

func (l *loop) post(ctx context.Context, defs []map[string]any) (message, int, error) {
	req := map[string]any{"model": l.spec.Model, "messages": l.msgs, "stream": false}
	if len(defs) > 0 {
		req["tools"] = defs
	}
	body, _ := json.Marshal(req)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.spec.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, 0, err
	}
	r.Header.Set("Content-Type", "application/json")
	if l.key != "" {
		r.Header.Set("Authorization", "Bearer "+l.key)
	}
	resp, err := l.hc.Do(r)
	if err != nil {
		return message{}, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		if len(b) > 512 {
			b = b[:512]
		}
		return message{}, resp.StatusCode, fmt.Errorf("model endpoint returned %d: %s", resp.StatusCode, b)
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				FunctionCall *struct { // legacy
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function_call"`
			} `json:"message"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if err := json.Unmarshal(b, &cr); err != nil || len(cr.Choices) == 0 {
		return message{}, resp.StatusCode, errors.New("model endpoint returned no choices")
	}
	l.usage.PromptTokens += cr.Usage.PromptTokens
	l.usage.CompletionTokens += cr.Usage.CompletionTokens
	l.usage.TotalTokens += cr.Usage.TotalTokens
	l.usage.Cost += cr.Usage.Cost
	cm := cr.Choices[0].Message
	out := message{Role: "assistant", Content: text(cm.Content)}
	add := func(id, name string, args json.RawMessage) {
		c := call{ID: id, Type: "function"}
		c.Function.Name = name
		c.Function.Arguments = argString(args)
		out.ToolCalls = append(out.ToolCalls, c)
	}
	for i, c := range cm.ToolCalls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		add(id, c.Function.Name, c.Function.Arguments)
	}
	if len(out.ToolCalls) == 0 && cm.FunctionCall != nil {
		add("call_0", cm.FunctionCall.Name, cm.FunctionCall.Arguments)
	}
	return out, resp.StatusCode, nil
}

// argString accepts arguments as a JSON string (OpenAI) or an object (some servers).
func argString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

// text reads content that is a string or a list of {text} parts.
func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var sb strings.Builder
		for _, p := range x {
			if m, ok := p.(map[string]any); ok {
				s, _ := m["text"].(string)
				sb.WriteString(s)
			}
		}
		return sb.String()
	}
	return ""
}

func (l *loop) allowed(server, name string) bool {
	full := "mcp__" + server + "__" + name
	for _, a := range l.spec.Allowed {
		if a == full || a == "mcp__"+server {
			return true
		}
	}
	return false
}

// connect lists each server's tools and keeps the allowed ones, named mcp__<server>__<tool>.
func (l *loop) connect(ctx context.Context) (map[string]*tool, func(), error) {
	tools := map[string]*tool{}
	var sessions []*mcp.ClientSession
	closeAll := func() {
		for _, s := range sessions {
			s.Close()
		}
	}
	servers := make([]string, 0, len(l.spec.MCP))
	for n := range l.spec.MCP {
		servers = append(servers, n)
	}
	sort.Strings(servers)
	for _, n := range servers {
		s := l.spec.MCP[n]
		var tr mcp.Transport
		switch {
		case s.URL != "" && len(s.Command) == 0:
			hc := &http.Client{Transport: headerTransport{l.hc.Transport, s.Headers}}
			tr = &mcp.StreamableClientTransport{Endpoint: s.URL, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}
		case s.URL == "" && len(s.Command) > 0:
			cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
			cmd.Env = []string{}
			for _, e := range []string{"PATH", "HOME", "LANG"} {
				if v, ok := os.LookupEnv(e); ok {
					cmd.Env = append(cmd.Env, e+"="+v)
				}
			}
			tr = &mcp.CommandTransport{Command: cmd}
		default:
			return nil, closeAll, fmt.Errorf("mcp server %q needs one URL or command", n)
		}
		sess, err := mcp.NewClient(&mcp.Implementation{Name: "siphon", Version: "1"}, nil).Connect(ctx, tr, nil)
		if err != nil {
			return nil, closeAll, fmt.Errorf("mcp server %q: %w", n, err)
		}
		sessions = append(sessions, sess)
		for t, err := range sess.Tools(ctx, nil) {
			if err != nil {
				return nil, closeAll, fmt.Errorf("mcp server %q: %w", n, err)
			}
			if !l.allowed(n, t.Name) {
				continue
			}
			full := "mcp__" + n + "__" + t.Name
			if tools[full] != nil {
				return nil, closeAll, fmt.Errorf("two MCP servers expose the tool name %q", full)
			}
			params := t.InputSchema
			if params == nil {
				params = map[string]any{"type": "object"}
			}
			tools[full] = &tool{sess: sess, name: t.Name, def: map[string]any{"name": full, "description": t.Description, "parameters": params}}
		}
	}
	return tools, closeAll, nil
}

type headerTransport struct {
	base http.RoundTripper
	h    map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.h {
		r.Header.Set(k, v)
	}
	if t.base == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	return t.base.RoundTrip(r)
}
