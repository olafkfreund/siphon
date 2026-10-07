package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeBridgeSandboxNone(t *testing.T) {
	exe, _ := os.Executable()
	o := AgentOptions{Timeout: time.Minute, Sandbox: SandboxOptions{Mode: "none"}, StateDir: t.TempDir(),
		MCP: map[string]MCPServer{"srv": {Command: []string{exe, "stub-mcp"}, Env: map[string]string{"TOKEN": "tok-123"}}, "other": {URL: "https://e.example/mcp"}}}
	tools, err := ProbeBridge(context.Background(), o, "srv")
	if err != nil || len(tools) != 1 || tools[0] != "check" {
		t.Fatalf("%v %v", tools, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "siphon-mcp-*")); len(entries) != 0 {
		t.Fatalf("bridge dir left: %v", entries)
	}
	if left, _ := os.ReadDir(filepath.Join(o.StateDir, "bridge-secrets")); len(left) != 0 {
		t.Fatalf("secrets left: %v", left)
	}
	for _, name := range []string{"other", "missing"} {
		if _, err := ProbeBridge(context.Background(), o, name); err == nil {
			t.Errorf("%s probed", name)
		}
	}
}

// The systemd path reaches the bridge's socket directly. The fake bridge never
// answers, so the cap fires; the unit is still stopped and nothing is left.
func TestProbeBridgeSystemdCapStopsBridge(t *testing.T) {
	dir := sockDir(t)
	f := newBridgeFake(t, dir)
	old := probeTimeout
	probeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })
	state := t.TempDir()
	o := AgentOptions{Sandbox: SandboxOptions{Mode: "systemd", Dir: dir, Egress: &EgressEnv{}}, Timeout: time.Minute, StateDir: state,
		MCP: map[string]MCPServer{"gh": envServer(nil)}}
	start := time.Now()
	_, err := ProbeBridge(context.Background(), o, "gh")
	if err == nil || strings.Contains(err.Error(), "tok-123") || time.Since(start) > 15*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	started, bridge, _ := f.snap()
	if len(started) != 1 || len(*f.stops) != 1 || (*f.stops)[0] != started[0] {
		t.Fatalf("started %v stops %v", started, *f.stops)
	}
	if b := bridge[0].Files["bridge.json"]; !strings.Contains(string(b), `"tools":["*"]`) {
		t.Errorf("probe did not ask for every tool: %s", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("bridge dir left behind: %v", entries)
	}
}

// A bridge that dies at once fails the probe with the bridge's masked error.
func TestProbeBridgeStartFailure(t *testing.T) {
	dir := sockDir(t)
	newBridgeFake(t, dir)
	startUnit = func(context.Context, string) error { return os.ErrInvalid }
	o := AgentOptions{Sandbox: SandboxOptions{Mode: "systemd", Dir: dir, Egress: &EgressEnv{}}, StateDir: t.TempDir(), MCP: map[string]MCPServer{"gh": envServer(nil)}}
	if _, err := ProbeBridge(context.Background(), o, "gh"); err == nil || !strings.Contains(err.Error(), "exited before it listened") {
		t.Fatalf("%v", err)
	}
}
