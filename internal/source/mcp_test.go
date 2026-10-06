package source

import (
	"context"
	"testing"

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
