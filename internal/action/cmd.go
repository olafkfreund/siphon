package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
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
	return runCommand(ctx, argv, opts, secrets)
}

func runCommand(ctx context.Context, argv []string, opts SandboxOptions, secrets []string) (int, []byte, error) {
	if len(argv) == 0 {
		return -1, nil, errors.New("empty command")
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
		argv = SandboxArgv(argv, opts.Timeout, opts.Credentials, opts.Unit)
	case "none":
	default:
		return -1, nil, fmt.Errorf("invalid sandbox mode %q", mode)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	maxExtra := 0
	for _, secret := range secrets {
		if len(secret) > maxExtra {
			maxExtra = len(secret)
		}
	}
	buf := &cappedBuffer{limit: 64<<10 + maxExtra}
	cmd.Stdout, cmd.Stderr = buf, buf
	err := cmd.Run()
	output := buf.Bytes()
	output = Mask(output, secrets)
	if len(output) > 64<<10 {
		output = output[:64<<10]
	}
	if err == nil {
		return 0, output, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), output, nil
	}
	return -1, output, err
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
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < b.limit {
		_, _ = b.Buffer.Write(p[:min(len(p), b.limit-b.Len())])
	}
	return n, nil
}
