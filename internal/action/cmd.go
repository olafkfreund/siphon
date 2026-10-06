package action

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
)

func Render(argv []string, data any) ([]string, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	if strings.Contains(argv[0], "{{") {
		return nil, errors.New("command name must not contain a template action")
	}
	out := make([]string, len(argv))
	for i, arg := range argv {
		t, err := template.New("arg").Option("missingkey=error").Parse(arg)
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, data); err != nil {
			return nil, err
		}
		out[i] = b.String()
		if strings.HasPrefix(out[i], "-") && !strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("argument %d introduced leading dash", i)
		}
	}
	return out, nil
}

func RunCmd(ctx context.Context, argv []string, opts SandboxOptions, secrets []string) (int, []byte, error) {
	exit, output, _, err := runCommand(ctx, argv, opts, secrets, nil, false)
	return exit, output, err
}

func runCommand(ctx context.Context, argv []string, opts SandboxOptions, secrets []string, stdin []byte, separateStdout bool) (int, []byte, []byte, error) {
	if len(argv) == 0 {
		return -1, nil, nil, errors.New("empty command")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	mode := opts.Mode
	if mode == "" {
		mode = "systemd"
	}
	switch mode {
	case "systemd":
		if opts.Unit == "" {
			// Named units let the NixOS polkit rule allow only agentgw-* transient units.
			id := make([]byte, 8)
			if _, err := rand.Read(id); err != nil {
				return -1, nil, nil, err
			}
			opts.Unit = "agentgw-run-" + hex.EncodeToString(id) + ".service"
		}
		argv = SandboxArgv(argv, opts)
	case "none":
	default:
		return -1, nil, nil, fmt.Errorf("invalid sandbox mode %q", mode)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Cancel = func() error {
		if mode == "systemd" {
			// Killing the systemd-run client does not stop the transient unit
			// (it belongs to PID 1); stop it explicitly so no action outlives us.
			_ = exec.Command(stopUnitArgv(opts.Unit)[0], stopUnitArgv(opts.Unit)[1:]...).Run()
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	maxExtra := 0
	for _, secret := range secrets {
		if len(secret) > maxExtra {
			maxExtra = len(secret)
		}
	}
	buf := &cappedBuffer{limit: 64<<10 + maxExtra}
	cmd.Stdout, cmd.Stderr = buf, buf
	var stdout *cappedBuffer
	if separateStdout {
		stdout = &cappedBuffer{limit: 1<<20 + maxExtra}
		cmd.Stdout = io.MultiWriter(buf, stdout)
	}
	err := cmd.Run()
	output := Mask(buf.Bytes(), secrets)
	if len(output) > 64<<10 {
		output = output[:64<<10]
	}
	var stdoutBytes []byte
	if stdout != nil {
		stdoutBytes = Mask(stdout.Bytes(), secrets)
		if len(stdoutBytes) > 1<<20 {
			stdoutBytes = stdoutBytes[:1<<20]
		}
	}
	if err == nil {
		return 0, output, stdoutBytes, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), output, stdoutBytes, nil
	}
	return -1, output, stdoutBytes, err
}

// Mask replaces known secrets in output, longest first.
func Mask(b []byte, secrets []string) []byte {
	secrets = append([]string(nil), secrets...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			b = bytes.ReplaceAll(b, []byte(secret), []byte("***"))
		}
	}
	return b
}

type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	mu    sync.Mutex
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.buf.Len() < b.limit {
		_, _ = b.buf.Write(p[:min(len(p), b.limit-b.buf.Len())])
	}
	return n, nil
}
