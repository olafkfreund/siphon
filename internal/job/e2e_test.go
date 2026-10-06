package job

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

const e2eConfig = `
server: { sandbox: none }
sources:
  factory:
    type: mcp
    command: [unused]          # replaced by the in-memory transport
    read: { resource: "test://value" }
rules:
  - name: high
    source: factory
    when: 'event.value > 10'
    on: edge
    action: { cmd: [echo, "value={{.event.value}}"] }
`

// TestRunOnceEdgeEndToEnd drives a real go-sdk MCP server through poll → rule → cmd job.
func TestRunOnceEdgeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var value atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	server.AddResource(&mcp.Resource{Name: "value", URI: "test://value"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI: "test://value", MIMEType: "application/json", Text: fmt.Sprintf(`{"value":%d}`, value.Load()),
			}}}, nil
		})

	cfg, err := config.Parse([]byte(e2eConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Unix(1_700_000_000, 0)
	p := &Pipeline{Cfg: cfg, Store: st, Now: func() time.Time { return now },
		MCPTransport: func(string) mcp.Transport {
			serverSide, clientSide := mcp.NewInMemoryTransports()
			if _, err := server.Connect(ctx, serverSide, nil); err != nil {
				t.Fatal(err)
			}
			return clientSide
		}}

	// value → expected total jobs after RunOnce
	steps := []struct{ value, jobs int64 }{
		{5, 0},  // false
		{20, 1}, // false→true fires
		{25, 1}, // still true: no refire
		{3, 1},  // true→false
		{30, 2}, // false→true fires again
	}
	for i, s := range steps {
		value.Store(s.value)
		now = now.Add(time.Minute)
		if err := p.RunOnce(ctx); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		var n int64
		if err := st.DB.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != s.jobs {
			t.Fatalf("step %d (value %d): %d jobs, want %d", i, s.value, n, s.jobs)
		}
	}

	// A cancelled run must not mark queued jobs failed (review H2).
	value.Store(5)
	now = now.Add(time.Minute)
	_ = p.RunOnce(ctx)
	value.Store(40)
	now = now.Add(time.Minute)
	if _, err := p.Tick(ctx, "factory"); err != nil {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if _, err := p.RunQueued(cctx); err == nil {
		t.Fatal("RunQueued on a cancelled context returned nil")
	}
	var queued int
	if err := st.DB.QueryRow(`SELECT count(*) FROM jobs WHERE state='queued'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued jobs after cancelled run = %d, want 1", queued)
	}
	if _, err := st.DB.Exec(`DELETE FROM jobs WHERE state='queued'`); err != nil {
		t.Fatal(err)
	}

	rows, err := st.DB.Query(`SELECT state, output FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []string{"value=20", "value=30"}
	for i := 0; rows.Next(); i++ {
		var state, out string
		if err := rows.Scan(&state, &out); err != nil {
			t.Fatal(err)
		}
		if state != "done" || strings.TrimSpace(out) != want[i] {
			t.Fatalf("job %d: state=%s output=%q, want done %q", i, state, out, want[i])
		}
	}
}
