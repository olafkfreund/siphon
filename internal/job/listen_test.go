package job

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

// Plan step 14 verify: a resource-updated notification makes Serve re-read
// the source long before the (1h) poll interval.
func TestServeListenHintTicksEarly(t *testing.T) {
	listenDebounce = 100 * time.Millisecond
	t.Cleanup(func() { listenDebounce = 5 * time.Second })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var value atomic.Int64
	value.Store(5)
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	server.AddResource(&mcp.Resource{Name: "v", URI: "test://v"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI: "test://v", MIMEType: "application/json", Text: fmt.Sprintf(`{"value":%d}`, value.Load()),
			}}}, nil
		})

	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db, workers: 1 }
sources:
  m: { type: mcp, command: [unused], read: { resource: "test://v" }, poll: 1h }
rules:
  - { name: high, source: m, when: 'event.value > 10', action: { cmd: [echo, hi] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := New(cfg, st, time.Now)
	p.MCPTransport = func(string) mcp.Transport {
		s, c := mcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, s, nil); err != nil {
			t.Error(err)
		}
		return c
	}
	served := make(chan error, 1)
	go func() { served <- p.Serve(ctx) }()

	time.Sleep(300 * time.Millisecond) // initial poll (value 5) and subscription
	value.Store(20)
	if err := server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "test://v"}); err != nil {
		t.Fatal(err)
	}
	for {
		var n int
		st.DB.QueryRow(`SELECT count(*) FROM jobs WHERE state='done'`).Scan(&n)
		if n == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("no job: the listen hint did not trigger an early poll")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	<-served
}
