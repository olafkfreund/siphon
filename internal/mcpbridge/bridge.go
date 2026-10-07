// Package mcpbridge runs one stdio MCP server with its secret env and serves
// it as streamable HTTP on a unix socket (`siphon mcp-bridge`). The agent never
// sees the env: it only reaches the socket, through a loopback forwarder.
package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Spec is the bridge's job: the server, env name -> file holding the value, and the socket to serve.
type Spec struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
	Socket  string            `json:"socket"`
}

// Run blocks until ctx ends or the server exits. ponytail: relays tools only
// (the tool list is read once at start); add resources/prompts when an agent needs them.
func Run(ctx context.Context, spec Spec) error {
	if len(spec.Command) == 0 || spec.Socket == "" {
		return errors.New("mcp-bridge: bad spec")
	}
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	for name, file := range spec.Env {
		v, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("mcp-bridge: env %s: %w", name, err)
		}
		cmd.Env = append(cmd.Env, name+"="+strings.TrimRight(string(v), "\n"))
	}
	cmd.Stderr = os.Stderr
	up, err := mcp.NewClient(&mcp.Implementation{Name: "siphon-bridge", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	defer up.Close()
	srv := mcp.NewServer(&mcp.Implementation{Name: "siphon-bridge", Version: "1"}, nil)
	for t, err := range up.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return up.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
		})
	}
	os.Remove(spec.Socket)
	old := syscall.Umask(0o117) // socket 0660: the run's group (siphon-io) may connect
	ln, err := net.Listen("unix", spec.Socket)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() { up.Wait(); close(done) }()
	go func() {
		select {
		case <-ctx.Done():
		case <-done: // the server died
		}
		hs.Close()
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
