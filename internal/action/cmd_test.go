package action

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	argv, err := Render([]string{"printf", "%s", "{{.Value}}"}, map[string]string{"Value": "hello world"})
	if err != nil || !reflect.DeepEqual(argv, []string{"printf", "%s", "hello world"}) {
		t.Fatalf("%q %v", argv, err)
	}
	if _, err := Render([]string{"rm", "{{.Value}}"}, map[string]string{"Value": "-rf"}); err == nil {
		t.Fatal("injected flag accepted")
	}
	if _, err := Render([]string{"{{.Value}}"}, map[string]string{"Value": "printf"}); err == nil || !strings.Contains(err.Error(), "command name") {
		t.Fatalf("templated command name accepted: %v", err)
	}
	argv, err = Render([]string{"printf", "{{.id}}"}, map[string]any{"id": int64(1700000000)})
	if err != nil || argv[1] != "1700000000" {
		t.Fatalf("rendered integer: %q, %v", argv, err)
	}
}

func TestSandboxArgv(t *testing.T) {
	got := SandboxArgv([]string{"true"}, SandboxOptions{Timeout: 1500 * time.Millisecond, Credentials: map[string]string{"key": "/tmp/key"}})
	want := []string{"systemd-run", "--wait", "--pipe", "--collect", "--quiet", "--setenv=HOME=/tmp", "--property=DynamicUser=yes", "--property=ProtectSystem=strict", "--property=ProtectHome=yes", "--property=PrivateTmp=yes", "--property=NoNewPrivileges=yes", "--property=IPAddressDeny=169.254.0.0/16", "--property=RuntimeMaxSec=2s", "--property=LoadCredential=key:/tmp/key", "--", "true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestRunCmd(t *testing.T) {
	t.Setenv("AGW_SECRET_TEST", "x")
	exit, output, err := RunCmd(context.Background(), []string{"env"}, SandboxOptions{Mode: "none"}, nil)
	if err != nil || exit != 0 || strings.Contains(string(output), "AGW_SECRET_TEST=") {
		t.Fatalf("child inherited gateway secret: exit=%d output=%q err=%v", exit, output, err)
	}
	exit, output, err = RunCmd(context.Background(), []string{"printf", "token=%s", "secret"}, SandboxOptions{Mode: "none"}, []string{"secret"})
	if err != nil || exit != 0 || string(output) != "token=***" {
		t.Fatalf("exit=%d output=%q err=%v", exit, output, err)
	}
	exit, output, err = RunCmd(context.Background(), []string{"head", "-c", "70000", "/dev/zero"}, SandboxOptions{Mode: "none"}, nil)
	if err != nil || exit != 0 || len(output) != 64<<10 {
		t.Fatalf("exit=%d len=%d err=%v", exit, len(output), err)
	}
	if strings.Contains(string(output), "secret") {
		t.Fatal("unmasked output")
	}
	_, output, err = RunCmd(context.Background(), []string{"printf", "%s", "abcdef"}, SandboxOptions{Mode: "none"}, []string{"abc", "abcdef"})
	if err != nil || string(output) != "***" {
		t.Fatalf("overlapping secrets: %q, %v", output, err)
	}
}
