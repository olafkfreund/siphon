package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/siphon/internal/egress"
)

type echoIn struct {
	Text string `json:"text"`
}

func mcpServer(t *testing.T) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "ext", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Text}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "secret", Description: "not allowed"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "x"}}}, nil, nil
	})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(srv.Close)
	return srv
}

// stub is an OpenAI-compatible endpoint driven by a script of replies.
type stub struct {
	mu    sync.Mutex
	reqs  []map[string]any
	auths []string
	reply func(n int, req map[string]any) (int, string)
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	n := len(s.reqs)
	s.reqs = append(s.reqs, req)
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.mu.Unlock()
	code, body := s.reply(n, req)
	w.WriteHeader(code)
	fmt.Fprint(w, body)
}

const toolCallReply = `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"mcp__ext__echo","arguments":"{\"text\":\"hi\"}"}}]}}],"usage":{"total_tokens":5}}`
const finalReply = `{"choices":[{"message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`

func runSpec(t *testing.T, s Spec) (map[string]any, int, string) {
	var out bytes.Buffer
	code := run(context.Background(), s, &http.Client{}, &out)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	return m, code, out.String()
}

func TestToolLoop(t *testing.T) {
	mc := mcpServer(t)
	key := filepath.Join(t.TempDir(), "k")
	os.WriteFile(key, []byte("sk-SECRET\n"), 0o600)
	st := &stub{reply: func(n int, _ map[string]any) (int, string) {
		if n == 0 {
			return 200, toolCallReply
		}
		return 200, strings.Replace(finalReply, "the answer", "got sk-SECRET", 1)
	}}
	llm := httptest.NewServer(st)
	defer llm.Close()
	m, code, raw := runSpec(t, Spec{BaseURL: llm.URL, Model: "m", KeyFile: key, Prompt: "go",
		MCP: map[string]MCPServer{"ext": {URL: mc.URL}}, Allowed: []string{"mcp__ext__echo"}})
	if code != 0 || m["type"] != "result" || m["turns"] != float64(2) {
		t.Fatalf("code %d: %s", code, raw)
	}
	if strings.Contains(raw, "sk-SECRET") {
		t.Fatalf("key in output: %s", raw)
	}
	if st.auths[0] != "Bearer sk-SECRET" {
		t.Fatalf("auth %q", st.auths[0])
	}
	// The second request carries the tool result; the first lists only the allowed tool.
	tools := st.reqs[0]["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "mcp__ext__echo" {
		t.Fatalf("tools: %v", tools)
	}
	if b, _ := json.Marshal(st.reqs[1]["messages"]); !strings.Contains(string(b), "echo:hi") {
		t.Fatalf("no tool result: %s", b)
	}
}

func TestPlainFallback(t *testing.T) {
	mc := mcpServer(t)
	st := &stub{reply: func(n int, req map[string]any) (int, string) {
		if req["tools"] != nil {
			return 400, `{"error":"tools unsupported"}`
		}
		return 200, finalReply
	}}
	llm := httptest.NewServer(st)
	defer llm.Close()
	m, code, raw := runSpec(t, Spec{BaseURL: llm.URL, Model: "m", Prompt: "go",
		MCP: map[string]MCPServer{"ext": {URL: mc.URL}}, Allowed: []string{"mcp__ext__echo"}})
	if code != 0 || m["result"] != "the answer" || m["note"] == nil || len(st.reqs) != 2 {
		t.Fatalf("code %d reqs %d: %s", code, len(st.reqs), raw)
	}
}

func TestNoToolsIsPlain(t *testing.T) {
	st := &stub{reply: func(int, map[string]any) (int, string) { return 200, finalReply }}
	llm := httptest.NewServer(st)
	defer llm.Close()
	if _, code, raw := runSpec(t, Spec{BaseURL: llm.URL, Model: "m", Prompt: "go"}); code != 0 || st.reqs[0]["tools"] != nil {
		t.Fatalf("code %d: %s %v", code, raw, st.reqs[0])
	}
}

func TestTurnCap(t *testing.T) {
	mc := mcpServer(t)
	st := &stub{reply: func(int, map[string]any) (int, string) { return 200, toolCallReply }}
	llm := httptest.NewServer(st)
	defer llm.Close()
	m, code, _ := runSpec(t, Spec{BaseURL: llm.URL, Model: "m", Prompt: "go", MaxTurns: 3,
		MCP: map[string]MCPServer{"ext": {URL: mc.URL}}, Allowed: []string{"mcp__ext__echo"}})
	if code != 1 || m["is_error"] != true || len(st.reqs) != 3 {
		t.Fatalf("code %d reqs %d: %v", code, len(st.reqs), m)
	}
}

func TestDisallowedToolCallRefused(t *testing.T) {
	mc := mcpServer(t)
	st := &stub{reply: func(int, map[string]any) (int, string) {
		return 200, strings.Replace(toolCallReply, "mcp__ext__echo", "mcp__ext__secret", 1)
	}}
	llm := httptest.NewServer(st)
	defer llm.Close()
	m, code, _ := runSpec(t, Spec{BaseURL: llm.URL, Model: "m", Prompt: "go",
		MCP: map[string]MCPServer{"ext": {URL: mc.URL}}, Allowed: []string{"mcp__ext__echo"}})
	if code != 1 || !strings.Contains(m["result"].(string), "malformed tool call") {
		t.Fatalf("code %d: %v", code, m)
	}
	if b, _ := json.Marshal(st.reqs[0]["tools"]); strings.Contains(string(b), "secret") {
		t.Fatalf("disallowed tool was sent: %s", b)
	}
}

func TestLegacyFunctionCallAndObjectArgs(t *testing.T) {
	mc := mcpServer(t)
	st := &stub{reply: func(n int, _ map[string]any) (int, string) {
		if n == 0 {
			return 200, `{"choices":[{"message":{"content":"","function_call":{"name":"mcp__ext__echo","arguments":{"text":"old"}}}}],"extra":1}`
		}
		return 200, finalReply
	}}
	llm := httptest.NewServer(st)
	defer llm.Close()
	if _, code, raw := runSpec(t, Spec{BaseURL: llm.URL, Model: "m", Prompt: "go",
		MCP: map[string]MCPServer{"ext": {URL: mc.URL}}, Allowed: []string{"mcp__ext"}}); code != 0 {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestCostCap(t *testing.T) {
	st := &stub{reply: func(int, map[string]any) (int, string) {
		return 200, `{"choices":[{"message":{"content":"x"}}],"usage":{"cost":2.5}}`
	}}
	llm := httptest.NewServer(st)
	defer llm.Close()
	if _, code, _ := runSpec(t, Spec{BaseURL: llm.URL, Model: "m", Prompt: "go", MaxCostUSD: 1}); code != 1 {
		t.Fatal("cost cap not enforced")
	}
}

func TestTrimAndToolResultCap(t *testing.T) {
	l := &loop{}
	for i := 0; i < 10; i++ {
		l.msgs = append(l.msgs, message{Role: "tool", Content: strings.Repeat("x", 64<<10)})
	}
	if err := l.trim(); err != nil {
		t.Fatal(err)
	}
	if l.msgs[0].Content != "[trimmed]" || len(l.msgs[9].Content) != 64<<10 {
		t.Fatal("oldest results must go first")
	}
}

// The dialer must tunnel both http and https targets through the real egress proxy.
func TestConnectDialerThroughProxy(t *testing.T) {
	p := egress.New("127.0.0.1:0", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	defer func() { cancel(); <-done }()
	for i := 0; p.Addr() == ""; i++ {
		if i > 500 {
			t.Fatal("proxy did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	plain, tls := httptest.NewServer(h), httptest.NewTLSServer(h)
	defer plain.Close()
	defer tls.Close()
	var entries []egress.Entry
	for _, s := range []*httptest.Server{plain, tls} {
		u, _ := url.Parse(s.URL)
		var port int
		fmt.Sscan(u.Port(), &port)
		entries = append(entries, egress.Entry{Host: u.Hostname(), Port: port, AllowPrivate: true})
	}
	proxyURL, blocked, _ := p.Register(entries)
	t.Setenv("HTTPS_PROXY", proxyURL)
	c := newClient()
	c.Transport.(*http.Transport).TLSClientConfig = tls.Client().Transport.(*http.Transport).TLSClientConfig
	for _, s := range []*httptest.Server{plain, tls} {
		resp, err := c.Get(s.URL)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v", s.URL, err)
		}
		resp.Body.Close()
	}
	if _, err := c.Get("http://other.example:80/"); err == nil {
		t.Fatal("unlisted host reached")
	}
	if len(blocked()) != 1 {
		t.Fatalf("blocked: %v", blocked())
	}
}
