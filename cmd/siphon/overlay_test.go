package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

const overlayBase = `server: { sandbox: none, db: s.db }
sources:
  a: { type: webhook, secret: "env:AGW_HOOK", signature: github }
rules:
  - { name: r1, source: a, when: "true", action: { cmd: [echo, one] } }
`

// overlayFixture writes the config file and a DB holding the given portal items.
func overlayFixture(t *testing.T, items ...store.ConfigItem) string {
	t.Helper()
	t.Setenv("AGW_HOOK", "s3cret")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "siphon.yaml")
	if err := os.WriteFile(cfgPath, []byte(overlayBase), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tx, _ := st.DB.Begin()
	for _, i := range items {
		if err := store.PutConfigItem(tx, i.Kind, i.Name, i.YAML, i.Deleted, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	return cfgPath
}

func capture(t *testing.T, f func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b), err
}

var portalRule = store.ConfigItem{Kind: "rules", Name: "r2", YAML: `{ source: a, when: "true", action: { cmd: [echo, two] } }`}

func TestConfigExportAndFileOnly(t *testing.T) {
	p := overlayFixture(t, portalRule)
	out, err := capture(t, func() error { return configExport([]string{"-config", p}) })
	if err != nil || !strings.Contains(out, "r1") || !strings.Contains(out, "r2") {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg, _, err := load("t", []string{"-config", p})
	if err != nil || len(cfg.Rules) != 2 {
		t.Fatalf("overlay not applied: %v %v", cfg, err)
	}
	cfg, _, err = load("t", []string{"-config", p, "-file-only"})
	if err != nil || len(cfg.Rules) != 1 {
		t.Fatalf("-file-only applied the overlay: %v %v", cfg, err)
	}
}

func TestValidateReportsBadOverlay(t *testing.T) {
	p := overlayFixture(t, store.ConfigItem{Kind: "rules", Name: "bad", YAML: `{ source: nope, when: "true", action: { cmd: [x] } }`})
	if err := validate([]string{"-config", p}); err == nil {
		t.Fatal("validate must fail on an invalid overlay")
	}
	if err := validate([]string{"-config", p, "-file-only"}); err != nil {
		t.Fatalf("file alone is valid: %v", err)
	}
}

func TestServeFallback(t *testing.T) {
	p := overlayFixture(t)
	bad := config.Item{Kind: "rules", Name: "bad", YAML: `{ source: nope, when: "true", action: { cmd: [x] } }`}
	good := config.Item{Kind: "rules", Name: "r2", YAML: `{ source: a, when: "true", action: { cmd: [echo, two] } }`}
	gone := config.Item{Kind: "rules", Name: "r1", Deleted: true}

	cfg, banner, err := pickConfig(p, []config.Item{good, bad})
	if err != nil || len(cfg.Rules) != 1 || cfg.Rules[0].Name != "r1" || !strings.HasPrefix(banner, "Portal edits could not be applied: ") || !strings.HasSuffix(banner, "running on the file with its deletions only") {
		t.Fatalf("fallback: %v %q %v", cfg, banner, err)
	}
	// What the operator deleted stays deleted when the rest of the overlay is bad.
	cfg, banner, err = pickConfig(p, []config.Item{gone, good, bad})
	if err != nil || len(cfg.Rules) != 0 || banner == "" {
		t.Fatalf("tombstone lost: %v %q %v", cfg, banner, err)
	}
	cfg, banner, err = pickConfig(p, []config.Item{good})
	if err != nil || len(cfg.Rules) != 2 || banner != "" {
		t.Fatalf("valid overlay: %v %q %v", cfg, banner, err)
	}
}
