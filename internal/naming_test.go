package internal_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestNoStragglers fails on the old name in tracked files outside the allowlist.
func TestNoStragglers(t *testing.T) {
	out, err := exec.Command("git", "-C", "..", "ls-files", "-z").Output()
	if err != nil {
		t.Skip("git unavailable:", err)
	}
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if allowedFile(f) {
			continue
		}
		b, err := os.ReadFile("../" + f)
		if err != nil {
			continue // deleted in the working tree
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(strings.ToLower(line), "agentgw") && !strings.Contains(line, "legacy-name") { // legacy-name
				t.Errorf("%s:%d: old name: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func allowedFile(f string) bool {
	for _, p := range []string{"intent/", "spec/", "plan/", "research/"} {
		if strings.HasPrefix(f, p) {
			return true
		}
	}
	if f == "schema/agentgw.schema.json" { // legacy-name
		return true
	}
	// TODO(plan step 2/3): remove
	for _, p := range []string{"flake.nix", "nix/", "devenv.nix", "README.md", "docs/", ".gitignore"} {
		if f == p || strings.HasSuffix(p, "/") && strings.HasPrefix(f, p) {
			return true
		}
	}
	return false
}
