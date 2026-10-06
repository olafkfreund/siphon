package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	exit, output, _, _, err := runCommand(ctx, argv, opts, secrets, nil, false)
	return exit, output, err
}

// RunCmdSplit also returns stdout alone for callers that parse JSON output.
func RunCmdSplit(ctx context.Context, argv []string, opts SandboxOptions, secrets []string) (exit int, output, stdout []byte, err error) {
	exit, output, stdout, _, err = runCommand(ctx, argv, opts, secrets, nil, true)
	return
}

func runCommand(ctx context.Context, argv []string, opts SandboxOptions, secrets []string, stdin []byte, separateStdout bool) (int, []byte, []byte, map[int][]byte, error) {
	if len(argv) == 0 {
		return -1, nil, nil, nil, errors.New("empty command")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	mode := opts.Mode
	if mode == "" {
		mode = "systemd"
	}
	limit := opts.Timeout
	if mode == "systemd" {
		limit += 30 * time.Second // exec-job enforces Timeout inside the unit; this is the backstop
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, limit)
	defer cancel()
	maxExtra := 0
	for _, secret := range secrets {
		maxExtra = max(maxExtra, len(secret))
	}
	switch mode {
	case "systemd":
		exit, so, se, writeback, err := templateRun(ctx, opts.Dir, JobSpec{
			Argv: argv, Stdin: stdin, Env: opts.Env, Files: opts.Files,
			Writeback:  opts.Writeback,
			TimeoutSec: int(opts.Timeout / time.Second),
		})
		output := capBytes(Mask(append(so, se...), secrets), 64<<10)
		var stdoutBytes []byte
		if separateStdout {
			stdoutBytes = capBytes(Mask(so, secrets), 1<<20)
		}
		if opts.stderr != nil {
			*opts.stderr = capBytes(Mask(se, secrets), 1<<20)
		}
		return exit, output, stdoutBytes, writeback, err
	case "none":
	default:
		return -1, nil, nil, nil, fmt.Errorf("invalid sandbox mode %q", mode)
	}
	runDir, err := os.MkdirTemp("", "agentgw-action-")
	if err != nil {
		return -1, nil, nil, nil, err
	}
	defer os.RemoveAll(runDir)
	home := opts.home
	if home == "" && (len(opts.Files) != 0 || len(opts.Writeback) != 0) {
		home = filepath.Join(runDir, "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			return -1, nil, nil, nil, err
		}
	} else if home == "" {
		home = os.Getenv("HOME")
	}
	if len(opts.Files) != 0 {
		if err := writeJobFiles(home, opts.Files); err != nil {
			return -1, nil, nil, nil, err
		}
	}
	for _, name := range opts.Writeback {
		if _, err := relativeFile(name); err != nil {
			return -1, nil, nil, nil, err
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "LANG"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	for name, value := range opts.Env {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.Contains(value, "\x00") {
			return -1, nil, nil, nil, fmt.Errorf("bad env %q", name)
		}
		if name != "HOME" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	if home != "" {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	buf := &cappedBuffer{limit: 64<<10 + maxExtra}
	cmd.Stdout, cmd.Stderr = buf, buf
	var stderr *cappedBuffer
	if opts.stderr != nil {
		stderr = &cappedBuffer{limit: 1<<20 + maxExtra}
		cmd.Stderr = io.MultiWriter(buf, stderr)
	}
	var stdout *cappedBuffer
	if separateStdout {
		stdout = &cappedBuffer{limit: 1<<20 + maxExtra}
		cmd.Stdout = io.MultiWriter(buf, stdout)
	}
	err = cmd.Run()
	writebackErr := io.Writer(buf)
	if stderr != nil {
		writebackErr = io.MultiWriter(buf, stderr)
	}
	saveWritebacks(home, runDir, JobSpec{Files: opts.Files, Writeback: opts.Writeback}, writebackErr)
	writeback := readWritebacks(runDir, opts.Writeback)
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
	if stderr != nil {
		*opts.stderr = capBytes(Mask(stderr.Bytes(), secrets), 1<<20)
	}
	if err == nil {
		return 0, output, stdoutBytes, writeback, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), output, stdoutBytes, writeback, nil
	}
	return -1, output, stdoutBytes, writeback, err
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

func capBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}
