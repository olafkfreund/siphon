package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveConfigFallback(t *testing.T) {
	t.Chdir(t.TempDir())
	parse := func(args ...string) *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("config", "siphon.yaml", "")
		fs.Parse(args)
		return fs
	}
	if got := resolveConfig(parse(), "siphon.yaml"); got != "siphon.yaml" {
		t.Fatalf("neither file: %q", got)
	}
	os.WriteFile("agentgw.yaml", nil, 0o644)                                 // legacy-name
	if got := resolveConfig(parse(), "siphon.yaml"); got != "agentgw.yaml" { // legacy-name
		t.Fatalf("legacy only: %q", got)
	}
	if got := resolveConfig(parse("-config", "siphon.yaml"), "siphon.yaml"); got != "siphon.yaml" {
		t.Fatalf("explicit flag: %q", got)
	}
	os.WriteFile("siphon.yaml", nil, 0o644)
	if got := resolveConfig(parse(), "siphon.yaml"); got != "siphon.yaml" {
		t.Fatalf("both: %q", got)
	}
}

func TestArgv0Notice(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "siphon")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	alias := filepath.Join(dir, "agentgw") // legacy-name
	if err := os.Symlink(bin, alias); err != nil {
		t.Fatal(err)
	}
	const notice = "agentgw is now siphon; this alias goes away in v0.2.0" // legacy-name
	out, _ := exec.Command(alias, "version").CombinedOutput()
	if !strings.Contains(string(out), notice) {
		t.Fatalf("alias: no notice in %q", out)
	}
	out, _ = exec.Command(bin, "version").CombinedOutput()
	if strings.Contains(string(out), "alias") {
		t.Fatalf("siphon: unexpected notice in %q", out)
	}
}
