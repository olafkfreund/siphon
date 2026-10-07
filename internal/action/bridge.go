package action

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
		sock, port := "", bridgeBasePort+i
		var stopOne func()
		if o.Sandbox.Mode == "none" {
			sock, stopOne, err = bridgeProcess(ctx, o, s)
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
			sock, stopOne, err = bridgeUnit(ctx, o, s)
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
		mcp[name] = MCPServer{URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/mcp"}
	}
	o.MCP = mcp
	return forwards, direct, stopAll, nil
}

// bridgeSpec lays the server's env out as value files named env-<i> in the
// unit's private files (or in dir, sandbox none).
func bridgeSpec(s MCPServer, socket string, filePath func(string) string) (mcpbridge.Spec, map[string][]byte) {
	spec := mcpbridge.Spec{Command: s.Command, Env: map[string]string{}, Socket: socket}
	files := map[string][]byte{}
	for i, k := range slices.Sorted(maps.Keys(s.Env)) {
		f := "env-" + strconv.Itoa(i)
		spec.Env[k], files[f] = filePath(f), []byte(s.Env[k])
	}
	return spec, files
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
func bridgeUnit(ctx context.Context, o *AgentOptions, s MCPServer) (string, func(), error) {
	id, runDir, gid, err := newRunDir(o.Sandbox.Dir)
	if err != nil {
		return "", nil, err
	}
	sock := filepath.Join(runDir, "mcp.sock")
	spec, files := bridgeSpec(s, sock, FilePath)
	b, _ := json.Marshal(spec)
	files["bridge.json"] = b
	job := JobSpec{Argv: bridgeCommand(FilePath("bridge.json")), Files: files, TimeoutSec: timeoutSeconds(o.Timeout + time.Minute)}
	if s.Egress != nil {
		var extra []string
		job.Env, job.EgressSocket, extra = egressSetup(s.Egress, o.Sandbox.Mode, nil)
		o.Secrets = append(o.Secrets, extra...)
	}
	if err := writeJob(runDir, gid, job); err != nil {
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
	stop := func() { cancel(); <-done; os.RemoveAll(runDir) }
	if err := waitSocket(sock, done); err != nil {
		stop()
		return "", nil, fmt.Errorf("%w: %s", err, Mask(stderr, append(slices.Collect(maps.Values(s.Env)), o.Secrets...)))
	}
	return sock, stop, nil
}

// bridgeProcess is bridgeUnit for sandbox none: a child process in a private temp dir.
func bridgeProcess(ctx context.Context, o *AgentOptions, s MCPServer) (string, func(), error) {
	dir, err := os.MkdirTemp("", "siphon-mcp-")
	if err != nil {
		return "", nil, err
	}
	sock := filepath.Join(dir, "mcp.sock")
	spec, files := bridgeSpec(s, sock, func(f string) string { return filepath.Join(dir, f) })
	b, _ := json.Marshal(spec)
	files["bridge.json"] = b
	if err := writeJobFiles(dir, files); err != nil {
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
		os.RemoveAll(dir)
	}
	if err := waitSocket(sock, done); err != nil {
		stop()
		return "", nil, fmt.Errorf("%w: %s", err, Mask(out.Bytes(), append(slices.Collect(maps.Values(s.Env)), o.Secrets...)))
	}
	return sock, stop, nil
}
