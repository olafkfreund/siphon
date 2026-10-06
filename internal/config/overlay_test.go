package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const base = `# keep me
server:
  listen: ":9000"
sources:
  a:
    type: webhook   # trailing
    secret: env:S
rules:
  - name: r1
    source: a
    when: "true"
    action: {cmd: [echo, one]}
  - name: r2
    source: a
    when: "true"
    action: {cmd: [echo, two]}
`

func eff(t *testing.T, items ...Item) (string, map[Key]Provenance) {
	t.Helper()
	b, p, err := Effective([]byte(base), items)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), p
}

func TestOverlayKinds(t *testing.T) {
	out, p := eff(t,
		Item{Kind: "sources", Name: "b", YAML: "type: webhook\nsecret: env:S2"},
		Item{Kind: "agents", Name: "ag", YAML: "kind: claude"},
		Item{Kind: "routines", Name: "ro", YAML: "steps: []"},
		Item{Kind: "credentials", Name: "cr", YAML: "provider: x"},
		Item{Kind: "sources", Name: "a", YAML: "type: webhook\nsecret: env:S3"},
	)
	for _, want := range []string{"# keep me", "b:", "ag:", "ro:", "cr:", "env:S3"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "env:S\n") || strings.Contains(out, "# trailing") {
		t.Errorf("a not replaced:\n%s", out)
	}
	if strings.Index(out, "\n  a:") > strings.Index(out, "\n  b:") {
		t.Error("key order changed")
	}
	want := map[Key]Provenance{{"sources", "a"}: FromOverride, {"sources", "b"}: FromPortal, {"agents", "ag"}: FromPortal,
		{"routines", "ro"}: FromPortal, {"credentials", "cr"}: FromPortal, {"rules", "r1"}: FromFile, {"rules", "r2"}: FromFile}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("prov %v", p)
	}
}

func TestOverlayRules(t *testing.T) {
	out, p := eff(t,
		Item{Kind: "rules", Name: "r1", YAML: "source: a\nwhen: \"false\"\naction: {cmd: [echo, new]}"},  // replace in place, name filled
		Item{Kind: "rules", Name: "r3", YAML: "name: r3\nsource: a\nwhen: \"true\"\naction: {cmd: [x]}"}, // append
		Item{Kind: "rules", Name: "r2", Deleted: true},                                                   // tombstone a file rule
	)
	c, err := Parse([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Rules) != 2 || c.Rules[0].Name != "r1" || c.Rules[0].When != "false" || c.Rules[1].Name != "r3" {
		t.Fatalf("rules %+v", c.Rules)
	}
	if _, ok := p[Key{"rules", "r2"}]; ok || p[Key{"rules", "r1"}] != FromOverride || p[Key{"rules", "r3"}] != FromPortal {
		t.Fatalf("prov %v", p)
	}
}

func TestOverlayTombstoneAndEmpty(t *testing.T) {
	out, _ := eff(t, Item{Kind: "sources", Name: "a", Deleted: true})
	if c, err := Parse([]byte(out)); err != nil || len(c.Sources) != 0 {
		t.Fatalf("%v %v", c, err)
	}
	// no file at all: kinds are created
	b, _, err := Effective(nil, []Item{{Kind: "rules", Name: "n", YAML: "source: s"}})
	if err != nil || !strings.Contains(string(b), "name: n") {
		t.Fatalf("%s %v", b, err)
	}
}

func TestOverlayRefusesKinds(t *testing.T) {
	for _, k := range []string{"server", "limits", "units", "bogus"} {
		if _, _, err := Effective([]byte(base), []Item{{Kind: k, Name: "x", YAML: "a: 1"}}); err == nil {
			t.Errorf("%s accepted", k)
		}
		if _, _, err := Effective([]byte(base), []Item{{Kind: k, Name: "x", Deleted: true}}); err == nil {
			t.Errorf("%s tombstone accepted", k)
		}
	}
}

func TestOverlayRoundTrip(t *testing.T) {
	for _, f := range []string{"testdata/full.yaml", "testdata/bad.yaml"} {
		b, _ := os.ReadFile(f)
		out, _, err := Effective(b, nil)
		if err != nil {
			t.Fatal(err)
		}
		want, err1 := Parse(b)
		got, err2 := Parse(out)
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: %v %v", f, err1, err2)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: round trip differs", f)
		}
	}
}

func TestLoadWithOverlayResolvesDB(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte(base), 0o600)
	c, _, err := LoadWithOverlay(p, []Item{{Kind: "agents", Name: "ag", YAML: "kind: claude"}})
	if err != nil || c.Agents["ag"] == nil || c.Server.DB != filepath.Join(dir, "siphon.db") {
		t.Fatalf("%+v %v", c, err)
	}
}
