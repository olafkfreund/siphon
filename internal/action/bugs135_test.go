package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #135 C2: a cancelled unit that can't be stopped is reported.
func TestRunUnitStopFailureReported(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	stopUnits = func(context.Context, ...string) error { return errors.New("polkit said no") }
	ctx, cancel := context.WithCancel(context.Background())
	startUnit = func(ctx context.Context, unit string) error { cancel(); <-ctx.Done(); return ctx.Err() }
	_, _, err := RunCmd(ctx, []string{"true"}, SandboxOptions{Mode: "systemd", Dir: dir, Timeout: time.Minute}, nil)
	if err == nil || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("err = %v", err)
	}
}

// #135 A7: is_error with exit 0 is a failure.
func TestIsErrorExitZeroFails(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s' '{\"is_error\":true,\"result\":\"x\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := RunAgent(context.Background(), AgentOptions{Command: stub, Prompt: "p", WorkDir: filepath.Join(dir, "work"), Sandbox: SandboxOptions{Mode: "none"}})
	if err != nil || got.Exit != 1 {
		t.Fatalf("%+v err=%v", got, err)
	}
}
