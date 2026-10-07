package mcpbridge

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStub is the stdio server the bridge starts: one tool that reports its env.
func TestStub(t *testing.T) {
	if os.Getenv("STUB_MCP") == "" {
		t.Skip("helper process")
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "stub", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "env", Description: "reads TOKEN"}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Name + "=" + os.Getenv(in.Name)}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "other", Description: "not allowed"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "other ran"}}}, nil, nil
	})
	s.Run(context.Background(), &mcp.StdioTransport{})
	os.Exit(0)
}

// bearer adds the bridge's token to every request.
type bearer struct{ rt http.RoundTripper }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer tok-bridge")
	return b.rt.RoundTrip(r)
}

func TestBridgeRelaysToolsOverSocket(t *testing.T) {
	t.Setenv("OUTER", "must-not-leak")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secrets"), []byte(`{"STUB_MCP":"1","TOKEN":"tok-123"}`), 0o600)
	sock := filepath.Join(dir, "mcp.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Spec{
			Command: []string{os.Args[0], "-test.run=^TestStub$"},
			Socket:  sock, Token: "tok-bridge", Tools: []string{"env"}, SecretsFile: filepath.Join(dir, "secrets"),
		})
	}()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	// No token, or a wrong one: 401 before anything is served.
	for _, auth := range []string{"", "Bearer wrong", "tok-bridge"} {
		req, _ := http.NewRequest("POST", "http://127.0.0.1:3200/mcp", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if resp, err := (&http.Client{Transport: tr}).Do(req); err != nil || resp.StatusCode != 401 {
			t.Fatalf("auth %q: %v %v", auth, resp, err)
		}
	}
	hc := &http.Client{Transport: bearer{tr}}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:3200/mcp", HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	n := 0
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil || tool.Name != "env" {
			t.Fatalf("tools: %v %v", tool, err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("%d tools (only the allowed one is served)", n)
	}
	if res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "other"}); err == nil && !res.IsError {
		t.Fatalf("a disallowed tool was called: %v", res)
	}
	call := func(name string) string {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "env", Arguments: map[string]any{"name": name}})
		if err != nil || len(res.Content) != 1 {
			t.Fatalf("call: %v %v", res, err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}
	if got := call("TOKEN"); got != "TOKEN=tok-123" {
		t.Errorf("env value (trailing newline trimmed): %q", got)
	}
	if got := call("OUTER"); strings.Contains(got, "must-not-leak") {
		t.Errorf("server inherited the bridge's env: %q", got)
	}
	sess.Close()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}
