package source

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPResource(t *testing.T) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	server.AddResource(&mcp.Resource{Name: "value", URI: "test://value"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "test://value", MIMEType: "application/json", Text: `{"value":42}`}}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx, serverSide)
	ev, err := (MCP{Options: MCPOptions{Name: "mcp", Resource: "test://value"}, Transport: clientSide}).Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Data.(map[string]any)["value"] != int64(42) {
		t.Fatalf("%+v", ev)
	}
}

func TestMCPListen(t *testing.T) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	subscribed := make(chan string, 1)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{
		SubscribeHandler: func(_ context.Context, req *mcp.SubscribeRequest) error {
			subscribed <- req.Params.URI
			return nil
		},
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	server.AddResource(&mcp.Resource{Name: "value", URI: "test://value"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go server.Run(ctx, serverSide)
	changed := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- (MCP{Options: MCPOptions{Resource: "test://value"}, Transport: clientSide}).Listen(ctx, func() {
			select { // never block the client: resent updates may arrive twice
			case changed <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case uri := <-subscribed:
		if uri != "test://value" {
			t.Fatalf("subscribed to %q", uri)
		}
	case err := <-done:
		t.Fatalf("Listen ended before subscription: %v", err)
	case <-ctx.Done():
		t.Fatal("subscription timed out")
	}
	select {
	case <-changed:
	case err := <-done:
		t.Fatalf("Listen ended before initial read: %v", err)
	case <-ctx.Done():
		t.Fatal("initial read timed out")
	}
	// The client's Subscribe can return before the server has recorded the
	// subscription (2026-07-28 protocol), and an update sent in that window
	// reaches no one; so resend until one arrives.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for updated := false; !updated; {
		if err := server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "test://value"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
			updated = true
		case err := <-done:
			t.Fatalf("Listen ended before update: %v", err)
		case <-ctx.Done():
			t.Fatal("update timed out")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Listen did not stop on cancellation")
	}
}

func TestMCPListenConnectTimeout(t *testing.T) {
	_, clientSide := mcp.NewInMemoryTransports()
	start := time.Now()
	err := (MCP{Options: MCPOptions{Resource: "test://value", Timeout: 20 * time.Millisecond}, Transport: clientSide}).Listen(context.Background(), func() {})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("Listen returned %v after %s", err, time.Since(start))
	}
}

func TestMCPListenUnsupported(t *testing.T) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	server.AddResource(&mcp.Resource{Name: "value", URI: "test://value"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go server.Run(ctx, serverSide)
	if err := (MCP{Options: MCPOptions{Resource: "test://value"}, Transport: clientSide}).Listen(ctx, func() {}); !errors.Is(err, ErrListenUnsupported) {
		t.Fatalf("got %v, want ErrListenUnsupported", err)
	}
	if err := (MCP{Options: MCPOptions{Tool: "read"}}).Listen(ctx, func() {}); !errors.Is(err, ErrListenUnsupported) {
		t.Fatalf("tool: got %v, want ErrListenUnsupported", err)
	}
}

func TestMCPListenHTTPTransport(t *testing.T) {
	s := MCP{Options: MCPOptions{URL: "https://example.test", Resource: "test://value"}}
	transport := s.transport(context.Background(), true).(*mcp.StreamableClientTransport)
	if transport.HTTPClient.Timeout != 0 || transport.DisableStandaloneSSE {
		t.Fatalf("listen stream is bounded or notifications disabled: %+v", transport)
	}
	if _, ok := transport.HTTPClient.Transport.(*http.Transport); !ok {
		t.Fatalf("listen stream has a body cap: %T", transport.HTTPClient.Transport)
	}
	if transport.HTTPClient.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("listen stream follows redirects")
	}
}

func TestMCPDeclaredTool(t *testing.T) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	calls := 0
	server.AddTool(&mcp.Tool{Name: "read_only", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls++
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"ok":true}`}}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx, serverSide)
	ev, err := (MCP{Options: MCPOptions{Tool: "read_only"}, Transport: clientSide}).Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || ev.Data.(map[string]any)["ok"] != true {
		t.Fatalf("calls=%d event=%+v", calls, ev)
	}
}

func TestMCPStructuredContentPreservesInteger(t *testing.T) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "id", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// As the MCP spec asks (and go-sdk's typed tools do): structured
		// output plus its serialized JSON as TextContent.
		return &mcp.CallToolResult{
			StructuredContent: map[string]any{"id": json.Number("9007199254740993")},
			Content:           []mcp.Content{&mcp.TextContent{Text: `{"id":9007199254740993}`}},
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx, serverSide)
	ev, err := (MCP{Options: MCPOptions{Tool: "id"}, Transport: clientSide}).Poll(ctx)
	if err != nil || ev.Data.(map[string]any)["id"] != int64(9007199254740993) {
		t.Fatalf("event=%+v err=%v", ev, err)
	}
}

func TestMCPToolError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []mcp.Content
		check   func(string) bool
	}{
		{"text", []mcp.Content{&mcp.TextContent{Text: "boom: 403 ACL"}}, func(m string) bool {
			return strings.Contains(m, "MCP tool fail returned an error: boom: 403 ACL")
		}},
		{"capped", []mcp.Content{&mcp.TextContent{Text: "x" + strings.Repeat("é", 3000)}}, func(m string) bool {
			// the odd prefix puts byte 4096 mid-rune, so the cut backs off
			_, text, _ := strings.Cut(m, "an error: ")
			return utf8.ValidString(text) && strings.HasSuffix(text, "…") && len(text) <= 4096+len("…")
		}},
		{"empty", nil, func(m string) bool { return strings.HasSuffix(m, "an error: (no message)") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverSide, clientSide := mcp.NewInMemoryTransports()
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{IsError: true, Content: tc.content}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go server.Run(ctx, serverSide)
			_, err := (MCP{Options: MCPOptions{Tool: "fail"}, Transport: clientSide}).Poll(ctx)
			if err == nil || !tc.check(err.Error()) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// TestStubMCP is the stdio child of TestMCPStdioEnv: it records its env and serves one tool.
func TestStubMCP(t *testing.T) {
	out := os.Getenv("STUB_OUT")
	if out == "" {
		t.Skip("helper process")
	}
	os.WriteFile(out, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	server := mcp.NewServer(&mcp.Implementation{Name: "stub", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"ok":true}`}}}, nil, nil
	})
	server.Run(context.Background(), &mcp.StdioTransport{})
	os.Exit(0)
}

func TestMCPStdioEnv(t *testing.T) {
	t.Setenv("OUTER_SECRET", "must-not-leak")
	out := filepath.Join(t.TempDir(), "env")
	ev, err := (MCP{Options: MCPOptions{
		Name: "m", Tool: "ping", Timeout: 10 * time.Second,
		Command: []string{os.Args[0], "-test.run=^TestStubMCP$"},
		Env:     map[string]string{"STUB_OUT": out, "GITHUB_TOKEN": "tok-123"},
	}}).Poll(context.Background())
	if err != nil || ev.Data.(map[string]any)["ok"] != true {
		t.Fatalf("%v %+v", err, ev)
	}
	b, _ := os.ReadFile(out)
	got := string(b)
	if !strings.Contains(got, "GITHUB_TOKEN=tok-123") || strings.Contains(got, "must-not-leak") {
		t.Fatalf("child env:\n%s", got)
	}
}

func TestRedirectChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.Write([]byte("hi"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()
	rt := RedirectChallenge(http.DefaultTransport, srv.URL+"/mcp/x?q=1")
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp
	}
	r := get("/mcp/x")
	if r.StatusCode != 401 || r.Header.Get("Siphon-Redirected") != "1" ||
		r.Header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource/mcp/x"` {
		t.Fatalf("endpoint 302: %d %v", r.StatusCode, r.Header)
	}
	if r := get("/other"); r.StatusCode != 302 || r.Header.Get("Siphon-Redirected") != "" {
		t.Fatalf("other path: %d", r.StatusCode)
	}
	if r := get("/ok"); r.StatusCode != 200 {
		t.Fatalf("200: %d", r.StatusCode)
	}
}

func TestOneLine(t *testing.T) {
	if got := OneLine("a\n  b\tc", 300); got != "a b c" {
		t.Fatalf("%q", got)
	}
	if got := OneLine("x"+strings.Repeat("é", 200), 300); !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || len(got) > 300+len("…") {
		t.Fatalf("%q", got)
	}
}
