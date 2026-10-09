package internal_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestNoStragglers fails on the old name in tracked files outside the history folders.
func TestNoStragglers(t *testing.T) {
	old := "agent" + "gw" // split, so this file doesn't match itself
	out, err := exec.Command("git", "-C", "..", "ls-files", "-z").Output()
	if err != nil {
		t.Skip("git unavailable:", err)
	}
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if allowedFile(f) {
			continue
		}
		if strings.Contains(strings.ToLower(f), old) {
			t.Errorf("%s: old name in the file name", f)
		}
		b, err := os.ReadFile("../" + f)
		if err != nil {
			continue // deleted in the working tree
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(strings.ToLower(line), old) {
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
	return false
}
