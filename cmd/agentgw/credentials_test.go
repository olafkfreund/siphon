package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run captures stdout of credentials(args) with stdin fed from in.
func runCreds(t *testing.T, in string, args ...string) (string, error) {
	t.Helper()
	inR, inW, _ := os.Pipe()
	inW.WriteString(in)
	inW.Close()
	outR, outW, _ := os.Pipe()
	oi, oo := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	err := credentials(args)
	os.Stdin, os.Stdout = oi, oo
	outW.Close()
	b, _ := io.ReadAll(outR)
	return string(b), err
}

func TestCredentialsImportLs(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agentgw.yaml")
	os.WriteFile(cfg, []byte("credentials: {max: {provider: claude}, cx: {provider: codex}}\n"), 0o600)
	login := `{"claudeAiOauth":{"accessToken":"ACCESS-SECRET","refreshToken":"REFRESH-SECRET","expiresAt":1900000000000},"mcpOAuth":{"x":{"accessToken":"MCP-SECRET"}}}`
	out, err := runCreds(t, login, "import", "-config", cfg, "max")
	if err != nil || !strings.HasPrefix(out, "imported max (claude), expires 2030-") {
		t.Fatalf("import: %q %v", out, err)
	}
	ls, err := runCreds(t, "", "ls", "-config", cfg)
	if err != nil || !strings.Contains(ls, "max") || !strings.Contains(ls, "not imported") {
		t.Fatalf("ls: %q %v", ls, err)
	}
	for _, secret := range []string{"ACCESS-SECRET", "REFRESH-SECRET", "MCP-SECRET"} {
		if strings.Contains(out+ls, secret) {
			t.Fatalf("output leaks %s", secret)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "credentials", "max", "credentials.json")); strings.Contains(string(b), "MCP-SECRET") {
		t.Fatal("mcpOAuth stored")
	}
	if _, err := runCreds(t, "junk", "import", "-config", cfg, "cx"); err == nil {
		t.Fatal("bad shape accepted")
	}
	if _, err := runCreds(t, "tok", "import", "-config", cfg, "-token-stdin", "max"); err != nil {
		t.Fatal(err)
	}
}

func TestRulesTestEnvelope(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agentgw.yaml")
	os.WriteFile(cfg, []byte(`sources: {s: {type: http, url: "http://127.0.0.1/x"}}
rules:
  - name: r
    source: s
    when: 'headers["x-k"] == "v" && event.n == 1'
    action: { cmd: [echo, hi] }
`), 0o600)
	run := func(ev string) string {
		f := filepath.Join(dir, "ev.json")
		os.WriteFile(f, []byte(ev), 0o600)
		or, ow, _ := os.Pipe()
		old := os.Stdout
		os.Stdout = ow
		err := rulesTest(context.Background(), []string{"-config", cfg, "r", f})
		os.Stdout = old
		ow.Close()
		b, _ := io.ReadAll(or)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if out := run(`{"headers": {"X-K": "v"}, "event": {"n": 1}}`); !strings.Contains(out, `"argv"`) {
		t.Fatalf("envelope should fire: %s", out)
	}
	if out := run(`{"n": 1}`); strings.Contains(out, `"argv"`) {
		t.Fatalf("bare event has no headers: %s", out)
	}
}

func TestValidateVerboseEgress(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "agentgw.yaml")
	os.WriteFile(cfg, []byte("credentials: {c: {provider: codex}}\nagents: {a: {kind: codex}, b: {kind: codex, egress: {enabled: false}}}\n"), 0o600)
	or, ow, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = ow
	err := validate([]string{"-config", cfg, "-v"})
	os.Stdout = old
	ow.Close()
	b, _ := io.ReadAll(or)
	if err != nil || !strings.Contains(string(b), "agent a egress: chatgpt.com:443, auth.openai.com:443") || !strings.Contains(string(b), "agent b egress: off") {
		t.Fatalf("%q %v", b, err)
	}
}
