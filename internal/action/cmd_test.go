package action

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func TestRunCommandNoneWriteback(t *testing.T) {
	exit, _, stdout, wb, err := runCommand(context.Background(), []string{"sh", "-c", `cat "$HOME/.codex/auth.json"; printf new > "$HOME/.codex/auth.json"; printf '%s' "$HOME" >&2`},
		SandboxOptions{Mode: "none", Files: map[string][]byte{".codex/auth.json": []byte("old")}, Writeback: []string{".codex/auth.json"}}, nil, nil, true)
	if err != nil || exit != 0 || string(stdout) != "old" || string(wb[0]) != "new" {
		t.Fatalf("exit=%d stdout=%q writeback=%v err=%v", exit, stdout, wb, err)
	}
	_, output, _, _, err := runCommand(context.Background(), []string{"sh", "-c", `printf '%s' "$HOME"`}, SandboxOptions{Mode: "none"}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(output)); !os.IsNotExist(err) {
		t.Fatalf("temp HOME still exists: %q: %v", output, err)
	}
	for _, name := range []string{"../escape", "/absolute"} {
		if _, _, _, _, err := runCommand(context.Background(), []string{"true"}, SandboxOptions{Mode: "none", Files: map[string][]byte{name: []byte("x")}}, nil, nil, false); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	outside := t.TempDir()
	home := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := writeJobFiles(home, map[string][]byte{"linked/file": []byte("x")}); err == nil {
		t.Fatal("accepted symlinked parent")
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
