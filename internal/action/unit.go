package action

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// systemctlArgv builds the command for a unit action; tests replace it with a stub.
var systemctlArgv = func(unit string) []string {
	return []string{"systemctl", "start", "--wait", "--", unit}
}

// RunUnit starts an allowlisted systemd unit and waits for it to finish.
// The allowlist is checked again here (config validation is not trusted
// alone), and the NixOS polkit rule enforces it a third time. Privileged
// work lives in those reviewed units, never in the gateway itself.
func RunUnit(ctx context.Context, unit string, allowed []string, timeout time.Duration, secrets []string) (int, []byte, error) {
	if !slices.Contains(allowed, unit) {
		return -1, nil, fmt.Errorf("unit %q is not in the units allowlist", unit)
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	// systemctl itself is the client; the unit runs under its own config, so
	// no extra sandbox wraps this call.
	exit, out, _, _, err := runCommand(ctx, systemctlArgv(unit), SandboxOptions{Mode: "none", Timeout: timeout}, secrets, nil, false)
	return exit, out, err
}
