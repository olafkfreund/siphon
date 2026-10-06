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

func TestRunAgentMasksRefreshedToken(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	token := "refreshed-token-1234567890"
	script := `#!/bin/sh
printf '%s' '{"id_token":"refreshed-token-1234567890"}' > "$CODEX_HOME/auth.json"
printf '%s' 'refreshed-token-1234567890'
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := RunAgent(context.Background(), AgentOptions{Kind: "codex", Command: stub, Prompt: "p", WorkDir: filepath.Join(dir, "home"), Sandbox: SandboxOptions{Mode: "none"}, CredFiles: map[string][]byte{"auth.json": []byte(`{"OPENAI_API_KEY":"old-key-12345678901234"}`)}})
	if err != nil || got.Exit != 0 || !strings.Contains(string(got.Writeback["auth.json"]), token) {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	for _, s := range []string{string(got.Output), string(got.Stdout), got.Result} {
		if strings.Contains(s, token) || s != "***" {
			t.Fatalf("unmasked refreshed token: %q", s)
		}
	}
}

func TestRunAgentMasksRefreshedTokenAcrossOutputCap(t *testing.T) {
	for _, mode := range []string{"none", "systemd"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			if mode == "systemd" {
				fakeSystemd(t, dir)
			}
			stub := filepath.Join(dir, "codex")
			token := "refreshed-token-1234567890"
			script := `#!/bin/sh
printf '%s' '{"id_token":"refreshed-token-1234567890"}' > "$CODEX_HOME/auth.json"
head -c 65530 /dev/zero | tr '\000' x
printf '%s' 'refreshed-token-1234567890'
`
			if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			got, err := RunAgent(context.Background(), AgentOptions{Kind: "codex", Command: stub, Prompt: "p", WorkDir: filepath.Join(dir, "home"), Sandbox: SandboxOptions{Mode: mode, Dir: dir}, CredFiles: map[string][]byte{"auth.json": []byte(`{"OPENAI_API_KEY":"old-key-12345678901234"}`)}})
			if err != nil || got.Exit != 0 || !strings.Contains(string(got.Writeback["auth.json"]), token) {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			for _, s := range []string{string(got.Output), string(got.Stdout), got.Result} {
				if strings.Contains(s, token[:10]) || !strings.HasSuffix(s, "***") {
					t.Fatalf("token leaked or was truncated: tail=%q", s[max(0, len(s)-40):])
				}
			}
			if len(got.Output) != 65533 {
				t.Fatalf("output length=%d", len(got.Output))
			}
		})
	}
}

func TestTokenStringsMasksEveryLongJSONValue(t *testing.T) {
	var secrets []string
	tokenStrings([]byte(`{"id_token":"id-token-1234567890123456","nested":{"OPENAI_API_KEY":"openai-key-1234567890123456","short":"public"}}`), &secrets)
	got := string(Mask([]byte("id-token-1234567890123456 openai-key-1234567890123456 public"), secrets))
	if got != "*** *** public" {
		t.Fatalf("masked=%q", got)
	}
}
