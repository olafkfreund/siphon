// Package mcpbridge runs one stdio MCP server with its secret env and serves
// it as streamable HTTP on a unix socket (`siphon mcp-bridge`). The agent never
// sees the env: it only reaches the socket, through a loopback forwarder.
package mcpbridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Spec is the bridge's job: the server, the socket to serve, the bearer token
// every request needs, and the tools it may serve ("*" = all). Secret env is
// never in the spec: it is JSON {NAME: value} in SecretsFile, or when that is
// empty in $CREDENTIALS_DIRECTORY/bridge (systemd LoadCredential).
type Spec struct {
	Command     []string `json:"command"`
	Socket      string   `json:"socket"`
	Token       string   `json:"token"`
	Tools       []string `json:"tools"`
	SecretsFile string   `json:"secrets_file,omitempty"`
}

// Run blocks until ctx ends or the server exits. ponytail: relays tools only
// (the tool list is read once at start); add resources/prompts when an agent needs them.
func Run(ctx context.Context, spec Spec) error {
	if len(spec.Command) == 0 || spec.Socket == "" || spec.Token == "" {
		return errors.New("mcp-bridge: bad spec")
	}
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	secrets := spec.SecretsFile
	if secrets == "" {
		secrets = filepath.Join(os.Getenv("CREDENTIALS_DIRECTORY"), "bridge")
	}
	raw, err := os.ReadFile(secrets)
	if err != nil {
		return fmt.Errorf("mcp-bridge: secrets: %w", err)
	}
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("mcp-bridge: secrets: %w", err)
	}
	for name, v := range env {
		cmd.Env = append(cmd.Env, name+"="+v)
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
		if !slices.Contains(spec.Tools, "*") && !slices.Contains(spec.Tools, t.Name) {
			continue
		}
		srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return up.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
		})
	}
	os.Remove(spec.Socket)
	// Listen on a temporary name, give the socket the run dir's group
	// (siphon-io: run dirs aren't setgid, so a new file would get this unit's
	// own group), then rename it into place. Whoever waits for spec.Socket
	// only ever sees a socket the agent's unit may connect to.
	tmp := spec.Socket + ".tmp"
	os.Remove(tmp)
	old := syscall.Umask(0o117) // socket 0660
	ln, err := net.Listen("unix", tmp)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false) // the path moves; it is removed below
	}
	if fi, err := os.Stat(filepath.Dir(spec.Socket)); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(tmp, -1, int(st.Gid)); err != nil && !errors.Is(err, syscall.EPERM) {
				ln.Close()
				return err
			}
		}
	}
	if err := os.Rename(tmp, spec.Socket); err != nil {
		ln.Close()
		return err
	}
	defer os.Remove(spec.Socket)
	mux := http.NewServeMux()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	want := []byte("Bearer " + spec.Token)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
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
