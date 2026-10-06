package action

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunUnit(t *testing.T) {
	var got []string
	orig := systemctlArgv
	systemctlArgv = func(unit string) []string {
		got = orig(unit)
		return []string{"echo", "started", unit} // stub: never touch the host's systemd
	}
	t.Cleanup(func() { systemctlArgv = orig })

	exit, out, err := RunUnit(context.Background(), "nix-gc.service", []string{"nix-gc.service"}, time.Second, nil)
	if err != nil || exit != 0 || strings.TrimSpace(string(out)) != "started nix-gc.service" {
		t.Fatalf("exit=%d out=%q err=%v", exit, out, err)
	}
	if strings.Join(got, " ") != "systemctl start --wait -- nix-gc.service" {
		t.Fatalf("argv %q", got)
	}

	// Runtime re-check: a unit outside the allowlist never reaches systemctl.
	got = nil
	if _, _, err := RunUnit(context.Background(), "sshd.service", []string{"nix-gc.service"}, time.Second, nil); err == nil || got != nil {
		t.Fatalf("non-allowlisted unit: err=%v argv=%q", err, got)
	}
}
