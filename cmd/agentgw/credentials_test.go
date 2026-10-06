package main

import (
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
