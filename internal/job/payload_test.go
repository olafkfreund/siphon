package job

import (
	"testing"

	"github.com/olafkfreund/MCP-AgentGateway/internal/action"
)

// Review H1: large integers must survive the job payload round trip.
func TestPayloadKeepsIntegers(t *testing.T) {
	pl, err := decodePayload(`{"action":{"cmd":["echo","{{.event.id}}"]},"env":{"event":{"id":1700000000}}}`)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := action.Render(pl.Action.Cmd, pl.Env)
	if err != nil {
		t.Fatal(err)
	}
	if argv[1] != "1700000000" {
		t.Fatalf("rendered %q, want 1700000000", argv[1])
	}
}
