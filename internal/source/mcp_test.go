package source

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

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
		done <- (MCP{Options: MCPOptions{Resource: "test://value"}, Transport: clientSide}).Listen(ctx, func() { changed <- struct{}{} })
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
	if err := server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "test://value"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case err := <-done:
		t.Fatalf("Listen ended before update: %v", err)
	case <-ctx.Done():
		t.Fatal("update timed out")
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
