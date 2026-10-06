package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAgentWritebackAndMask(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s' rotated > "$CODEX_HOME/auth.json"
printf '%s\n' 'refresh-token-secret' 'final answer'
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	o := AgentOptions{Kind: "codex", Command: stub, Prompt: "question", WorkDir: filepath.Join(dir, "work"), Sandbox: SandboxOptions{Mode: "none"}, CredFiles: map[string][]byte{"auth.json": []byte(`{"refresh_token":"refresh-token-secret"}`)}}
	got, err := RunAgent(context.Background(), o)
	if err != nil || got.Exit != 0 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if string(got.Writeback["auth.json"]) != "rotated" {
		t.Fatalf("writeback=%v", got.Writeback)
	}
	for _, s := range []string{string(got.Output), string(got.Stdout), got.Result} {
		if strings.Contains(s, "refresh-token-secret") {
			t.Fatalf("secret leaked: %q", s)
		}
	}
	if got.Result != "***\nfinal answer" {
		t.Fatalf("result=%q", got.Result)
	}
}

func TestAgentAPIKeyPathRejected(t *testing.T) {
	_, _, _, _, _, _, err := buildRun(AgentOptions{Prompt: "p", WorkDir: t.TempDir(), APIKeyFile: "/tmp/x;rm -rf ~"})
	if err == nil {
		t.Fatal("unsafe api key path accepted")
	}
}
