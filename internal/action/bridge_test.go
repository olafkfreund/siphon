package action

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/olafkfreund/siphon/internal/agentloop"
	"github.com/olafkfreund/siphon/internal/mcpbridge"
)

// TestMain lets the test binary stand in for `siphon agent-run`, `siphon
// mcp-bridge` and a stdio MCP server, so bridges run for real.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 {
		switch os.Args[1] {
		case "agent-run":
			var spec agentloop.Spec
			b, _ := os.ReadFile(os.Args[2])
			json.Unmarshal(b, &spec)
			os.Exit(agentloop.Run(context.Background(), spec, os.Stdout))
		case "mcp-bridge":
			var spec mcpbridge.Spec
			b, _ := os.ReadFile(os.Args[2])
			json.Unmarshal(b, &spec)
			if err := mcpbridge.Run(context.Background(), spec); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "stub-mcp" {
		s := mcp.NewServer(&mcp.Implementation{Name: "stub", Version: "1"}, nil)
		mcp.AddTool(s, &mcp.Tool{Name: "check"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			txt := "token-missing"
			if os.Getenv("TOKEN") == "tok-123" {
				txt = "token-ok"
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: txt}}}, nil, nil
		})
		s.Run(context.Background(), &mcp.StdioTransport{})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// bridgeFake runs fakeSystemd and replaces start for siphon-mcp@: it records
// the bridge's job.json, makes its socket, and "runs" until stopped. Agent
// units still go through ExecJob; their job.json is recorded too.
type bridgeFake struct {
	mu      sync.Mutex
	started []string
	bridge  []JobSpec
	agent   []JobSpec
	stops   *[]string
}

func newBridgeFake(t *testing.T, dir string) *bridgeFake {
	f := &bridgeFake{stops: fakeSystemd(t, dir)}
	agentStart := startUnit
	startUnit = func(ctx context.Context, unit string) error {
		id := strings.TrimSuffix(strings.TrimPrefix(unit, "siphon-mcp@"), ".service")
		isMCP := strings.HasPrefix(unit, "siphon-mcp@")
		if !isMCP {
			id = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(unit, "siphon-action-open@"), "siphon-action@"), ".service")
		}
		var spec JobSpec
		b, _ := os.ReadFile(filepath.Join(dir, id, "job.json"))
		json.Unmarshal(b, &spec)
		f.mu.Lock()
		if isMCP {
			f.started, f.bridge = append(f.started, unit), append(f.bridge, spec)
		} else {
			f.agent = append(f.agent, spec)
		}
		f.mu.Unlock()
		if !isMCP {
			return agentStart(ctx, unit)
		}
		ln, err := net.Listen("unix", filepath.Join(dir, id, "mcp.sock"))
		if err != nil {
			return err
		}
		defer ln.Close()
		<-ctx.Done()
		return ctx.Err()
	}
	return f
}

func (f *bridgeFake) snap() (started []string, bridge, agent []JobSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), append([]JobSpec(nil), f.bridge...), append([]JobSpec(nil), f.agent...)
}

func envServer(extra map[string]string) MCPServer {
	env := map[string]string{"TOKEN": "tok-123"}
	for k, v := range extra {
		env[k] = v
	}
	return MCPServer{Command: []string{"srv", "--flag"}, Env: env, Egress: &EgressEnv{ProxyURL: "http://u:pw@127.77.0.1:3128", Socket: "/run/egress.sock"}}
}

func TestBridgeUnitLifecycle(t *testing.T) {
	dir := t.TempDir()
	f := newBridgeFake(t, dir)
	o := AgentOptions{Sandbox: SandboxOptions{Mode: "systemd", Dir: dir}, Timeout: time.Minute,
		MCP: map[string]MCPServer{"gh": envServer(nil), "plain": {URL: "https://e.example/mcp"}}}
	forwards, direct, stop, err := startBridges(context.Background(), &o)
	if err != nil {
		t.Fatal(err)
	}
	started, bridge, _ := f.snap()
	if len(started) != 1 || !strings.HasPrefix(started[0], "siphon-mcp@") || !strings.HasSuffix(started[0], ".service") || len(started[0]) != len("siphon-mcp@")+16+len(".service") {
		t.Fatalf("started %v", started)
	}
	// "gh" sorts before "plain", so it takes the first port.
	sock := forwards[bridgeBasePort]
	if len(forwards) != 1 || filepath.Base(sock) != "mcp.sock" || strings.Join(direct, ",") != "127.0.0.1:3200" {
		t.Fatalf("forwards %v direct %v", forwards, direct)
	}
	if got := o.MCP["gh"]; got.URL != "http://127.0.0.1:3200/mcp" || got.Env != nil || got.Command != nil {
		t.Fatalf("not rewritten: %+v", got)
	}
	if o.MCP["plain"].URL != "https://e.example/mcp" {
		t.Fatalf("plain source touched: %+v", o.MCP["plain"])
	}
	job := bridge[0]
	if job.EgressSocket != "/run/egress.sock" || job.Env["HTTPS_PROXY"] == "" || !strings.HasSuffix(job.Argv[len(job.Argv)-1], "bridge.json") {
		t.Fatalf("bridge job %+v", job)
	}
	var spec mcpbridge.Spec
	json.Unmarshal(job.Files["bridge.json"], &spec)
	if string(job.Files["env-0"]) != "tok-123" || spec.Env["TOKEN"] != FilePath("env-0") || spec.Socket != sock || strings.Join(spec.Command, " ") != "srv --flag" {
		t.Fatalf("spec %+v files %v", spec, job.Files)
	}
	stop()
	if len(*f.stops) != 1 || (*f.stops)[0] != started[0] {
		t.Fatalf("stops %v, started %v", *f.stops, started)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("bridge dir left behind: %v", entries)
	}
}

func TestBridgeSocketNeverAppears(t *testing.T) {
	dir := t.TempDir()
	f := newBridgeFake(t, dir)
	_ = f
	startUnit = func(context.Context, string) error { return os.ErrInvalid } // the unit dies at once
	o := AgentOptions{Sandbox: SandboxOptions{Mode: "systemd", Dir: dir}, MCP: map[string]MCPServer{"gh": envServer(nil)}}
	if _, _, _, err := startBridges(context.Background(), &o); err == nil || !strings.Contains(err.Error(), "exited before it listened") || strings.Contains(err.Error(), "tok-123") {
		t.Fatalf("err %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("dir left behind: %v", entries)
	}
}

// The agent's own job carries the forwards and a config that points at the bridge, for each kind.
func TestAgentConfigRewrittenForBridge(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			f := newBridgeFake(t, dir)
			stub := filepath.Join(t.TempDir(), "agent")
			os.WriteFile(stub, []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --mcp-config ] && cat \"$2\"; shift; done\ncat \"$CODEX_HOME/config.toml\" 2>/dev/null\nenv\n"), 0o755)
			res, err := RunAgent(context.Background(), AgentOptions{
				Kind: kind, Command: stub, Prompt: "x", Timeout: 10 * time.Second, WorkDir: t.TempDir(),
				AllowedTools: []string{"mcp__gh__x"},
				Sandbox:      SandboxOptions{Mode: "systemd", Dir: dir},
				MCP:          map[string]MCPServer{"gh": envServer(nil)},
			})
			if err != nil || res.Exit != 0 {
				t.Fatalf("%v %+v", err, res)
			}
			out := string(res.Stdout)
			if !strings.Contains(out, "127.0.0.1:3200/mcp") || strings.Contains(out, "tok-123") || strings.Contains(out, "srv") {
				t.Fatalf("agent saw:\n%s", out)
			}
			_, _, agent := f.snap()
			if len(agent) != 1 || len(agent[0].Forwards) != 1 || agent[0].Forwards[3200] == "" {
				t.Fatalf("agent job forwards: %+v", agent)
			}
			if no := agent[0].Env["NO_PROXY"]; no != "127.0.0.1:3200" {
				t.Fatalf("NO_PROXY %q", no)
			}
			for name, b := range agent[0].Files {
				if strings.Contains(string(b), "tok-123") {
					t.Fatalf("secret in agent file %s", name)
				}
			}
		})
	}
	t.Run("model", func(t *testing.T) {
		o := AgentOptions{Kind: "model", Model: "m", BaseURL: "http://x", Prompt: "p", WorkDir: t.TempDir(),
			Sandbox: SandboxOptions{Mode: "systemd", Dir: t.TempDir()}, MCP: map[string]MCPServer{"gh": {URL: "http://127.0.0.1:3200/mcp"}}}
		_, _, _, files, _, _, err := buildRun(o, jobFilesDir)
		if err != nil || !strings.Contains(string(files["spec.json"]), "127.0.0.1:3200/mcp") || strings.Contains(string(files["spec.json"]), "tok-123") {
			t.Fatalf("%v %s", err, files["spec.json"])
		}
	})
}

func TestExecJobServesForwards(t *testing.T) {
	jobFilesDir = t.TempDir()
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	run := t.TempDir()
	job := filepath.Join(run, "job.json")
	b, _ := json.Marshal(JobSpec{Argv: []string{"sleep", "3"}, Forwards: map[int]string{port: sock}})
	os.WriteFile(job, b, 0o600)
	done := make(chan int, 1)
	go func() { done <- execJob(run, job, io.Discard, io.Discard) }()
	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(c, "ping")
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
	c.Close()
	syscallTerm(t)
	<-done
}

// sandbox none, end to end: a model agent calls a tool through the bridge; the
// stub server gets the env value, the agent's own spec and env never hold it.
func TestBridgeSandboxNoneEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(b))
		mu.Unlock()
		if n == 0 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"mcp__srv__check","arguments":"{}"}}]}}],"usage":{"total_tokens":5}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"total_tokens":3}}`)
	}))
	defer llm.Close()
	exe, _ := os.Executable()
	res, err := RunAgent(context.Background(), AgentOptions{
		Kind: "model", Model: "m", BaseURL: llm.URL, Prompt: "go", Timeout: 30 * time.Second, WorkDir: t.TempDir(),
		AllowedTools: []string{"mcp__srv__check"}, Sandbox: SandboxOptions{Mode: "none"},
		MCP:     map[string]MCPServer{"srv": {Command: []string{exe, "stub-mcp"}, Env: map[string]string{"TOKEN": "tok-123"}}},
		Secrets: []string{"tok-123"},
	})
	if err != nil || res.Exit != 0 {
		t.Fatalf("%v %+v %s", err, res, res.Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[1], "token-ok") {
		t.Fatalf("the server did not get its env through the bridge: %v", bodies)
	}
	for _, b := range bodies {
		if strings.Contains(b, "tok-123") {
			t.Fatalf("secret reached the agent: %s", b)
		}
	}
	if bytes.Contains(res.Stdout, []byte("tok-123")) {
		t.Fatal("secret in agent output")
	}
	if entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "siphon-mcp-*")); len(entries) != 0 {
		t.Fatalf("bridge dir left: %v", entries)
	}
}

func syscallTerm(t *testing.T) {
	t.Helper()
	// execJob stops on SIGTERM (its NotifyContext also keeps the test process alive).
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}
