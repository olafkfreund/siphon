package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/olafkfreund/siphon/internal/mcpbridge"
)

// bridgeBasePort is the first loopback port an agent unit forwards to a bridge.
const bridgeBasePort = 3200

// bridgeCommand is the bridge's argv; tests swap in a helper process.
var bridgeCommand = func(spec string) []string {
	exe, _ := os.Executable()
	return []string{exe, "mcp-bridge", spec}
}

// startBridges moves every stdio MCP server that has secret env out of the
// agent: each gets a bridge (a siphon-mcp@ unit, or a child in sandbox none)
// and o.MCP is rewritten to a loopback URL with no env. forwards is what the
// agent's exec-job must listen on (systemd mode). stop ends the bridges.
func startBridges(ctx context.Context, o *AgentOptions) (forwards map[int]string, direct []string, stop func(), err error) {
	var stops []func()
	stopAll := func() {
		for _, s := range slices.Backward(stops) {
			s()
		}
	}
	defer func() {
		if err != nil {
			stopAll()
		}
	}()
	mcp := maps.Clone(o.MCP)
	names := slices.Sorted(maps.Keys(mcp))
	for i, name := range names {
		s := mcp[name]
		if len(s.Env) == 0 || len(s.Command) == 0 {
			continue
		}
		if o.Sandbox.Mode != "none" && o.Sandbox.Egress == nil {
			return nil, nil, nil, fmt.Errorf("MCP server %s needs a restricted agent (egress enabled): its secrets can't be bridged into an open sandbox", name)
		}
		tok, terr := randomHex(32)
		if terr != nil {
			return nil, nil, nil, terr
		}
		o.Secrets = append(o.Secrets, tok)
		tools := bridgeTools(name, o.AllowedTools)
		sock, port := "", bridgeBasePort+i
		var stopOne func()
		if o.Sandbox.Mode == "none" {
			sock, stopOne, err = bridgeProcess(ctx, o, s, tok, tools)
			if err == nil {
				var ln net.Listener
				if ln, err = net.Listen("tcp", "127.0.0.1:0"); err == nil {
					port = ln.Addr().(*net.TCPAddr).Port
					go forward(ln, sock)
					prev := stopOne
					stopOne = func() { ln.Close(); prev() }
				} else {
					stopOne()
				}
			}
		} else {
			sock, stopOne, err = bridgeUnit(ctx, o, s, tok, tools)
			if err == nil {
				if forwards == nil {
					forwards = map[int]string{}
				}
				forwards[port] = sock
			}
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("mcp bridge for %q: %w", name, err)
		}
		stops = append(stops, stopOne)
		direct = append(direct, "127.0.0.1:"+strconv.Itoa(port))
		mcp[name] = MCPServer{URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/mcp", Headers: map[string]string{"Authorization": "Bearer " + tok}}
	}
	o.MCP = mcp
	return forwards, direct, stopAll, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// bridgeTools is the tool names the agent may use on server name, from
// allowed_tools (mcp__<name>__<tool>; mcp__<name> means all).
func bridgeTools(name string, allowed []string) []string {
	tools := []string{}
	for _, a := range allowed {
		if a == "mcp__"+name {
			return []string{"*"}
		}
		if t, ok := strings.CutPrefix(a, "mcp__"+name+"__"); ok && t != "" {
			tools = append(tools, t)
		}
	}
	return tools
}

// bridgeSecretsDir is where the daemon leaves each bridge's secrets for
// systemd's LoadCredential (siphon-mcp@ reads bridge-secrets/%i).
func bridgeSecretsDir(stateDir string) string { return filepath.Join(stateDir, "bridge-secrets") }

// writeBridgeSecrets writes env as JSON to <stateDir>/bridge-secrets/<id>
// (dir 0700 and never a symlink, file 0600 created exclusively) and returns its path.
func writeBridgeSecrets(stateDir, id string, env map[string]string) (string, error) {
	if stateDir == "" {
		return "", errors.New("mcp bridge: no state directory configured")
	}
	dir := bridgeSecretsDir(stateDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("mcp bridge: %s is not a plain directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	b, _ := json.Marshal(env)
	path := filepath.Join(dir, id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// CleanBridgeSecrets removes bridge secrets a crashed siphon left behind.
func CleanBridgeSecrets(stateDir string) {
	os.RemoveAll(bridgeSecretsDir(stateDir))
}

func bridgeSpec(socket, token string, tools []string) mcpbridge.Spec {
	return mcpbridge.Spec{Socket: socket, Token: token, Tools: tools}
}

func waitSocket(sock string, died <-chan struct{}) error {
	deadline := time.After(10 * time.Second)
	for {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-died:
			return fmt.Errorf("the bridge exited before it listened")
		case <-deadline:
			return fmt.Errorf("the bridge did not listen within 10s")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// bridgeUnit starts siphon-mcp@<id> for s and returns its socket.
func bridgeUnit(ctx context.Context, o *AgentOptions, s MCPServer, token string, tools []string) (string, func(), error) {
	id, runDir, gid, err := newRunDir(o.Sandbox.Dir)
	if err != nil {
		return "", nil, err
	}
	sock := filepath.Join(runDir, "mcp.sock")
	secrets, err := writeBridgeSecrets(o.StateDir, id, s.Env)
	if err != nil {
		os.RemoveAll(runDir)
		return "", nil, err
	}
	spec := bridgeSpec(sock, token, tools)
	spec.Command = s.Command
	b, _ := json.Marshal(spec)
	files := map[string][]byte{"bridge.json": b}
	job := JobSpec{Argv: bridgeCommand(FilePath("bridge.json")), Files: files, TimeoutSec: timeoutSeconds(o.Timeout + time.Minute)}
	if s.Egress != nil {
		var extra []string
		job.Env, job.EgressSocket, extra = egressSetup(s.Egress, o.Sandbox.Mode, nil)
		o.Secrets = append(o.Secrets, extra...)
	}
	if err := writeJob(runDir, gid, job); err != nil {
		os.Remove(secrets)
		os.RemoveAll(runDir)
		return "", nil, err
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	var stderr []byte
	go func() { // returns when the unit exits; cancel makes runUnit stop it
		_, _, stderr, _, _ = runUnit(rctx, "siphon-mcp@"+id+".service", runDir, job, 64<<10)
		close(done)
	}()
	stop := func() { cancel(); <-done; os.Remove(secrets); os.RemoveAll(runDir) }
	if err := waitSocket(sock, done); err != nil {
		stop()
		return "", nil, fmt.Errorf("%w: %s", err, Mask(stderr, append(slices.Collect(maps.Values(s.Env)), o.Secrets...)))
	}
	return sock, stop, nil
}

// bridgeProcess is bridgeUnit for sandbox none: a child process in a private temp dir.
func bridgeProcess(ctx context.Context, o *AgentOptions, s MCPServer, token string, tools []string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "siphon-mcp-")
	if err != nil {
		return "", nil, err
	}
	sock := filepath.Join(dir, "mcp.sock")
	id, err := randomHex(8)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	secrets, err := writeBridgeSecrets(o.StateDir, id, s.Env)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	spec := bridgeSpec(sock, token, tools)
	spec.Command, spec.SecretsFile = s.Command, secrets
	b, _ := json.Marshal(spec)
	if err := writeJobFiles(dir, map[string][]byte{"bridge.json": b}); err != nil {
		os.Remove(secrets)
		os.RemoveAll(dir)
		return "", nil, err
	}
	argv := bridgeCommand(filepath.Join(dir, "bridge.json"))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	if s.Egress != nil {
		env, _, extra := egressSetup(s.Egress, "none", nil)
		o.Secrets = append(o.Secrets, extra...)
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out cappedBuffer
	out.limit = 64 << 10
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		os.Remove(secrets)
		os.RemoveAll(dir)
		return "", nil, err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	stop := func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		os.Remove(secrets)
		os.RemoveAll(dir)
	}
	if err := waitSocket(sock, done); err != nil {
		stop()
		return "", nil, fmt.Errorf("%w: %s", err, Mask(out.Bytes(), append(slices.Collect(maps.Values(s.Env)), o.Secrets...)))
	}
	return sock, stop, nil
}
