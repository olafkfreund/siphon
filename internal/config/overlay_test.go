package config

import (
	"context"
	"errors"
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

func TestOverlayRuleNameMismatch(t *testing.T) {
	_, _, err := Effective([]byte(base), []Item{{Kind: "rules", Name: "r1", YAML: "name: other\nsource: a"}})
	if err == nil || !strings.Contains(err.Error(), `rule name "other" does not match item "r1"`) {
		t.Fatalf("%v", err)
	}
}

const anchored = `limits: &L { agent_runs_per_day: 5 }
sources:
  a: { type: http, url: "https://e.example", poll: 1m, read: { resource: r, args: *L } }
`

// A portal item must not rebind an anchor the file uses for limits.
func TestOverlayRefusesAnchorsAndAliases(t *testing.T) {
	for _, y := range []string{
		`{type: http, url: "https://e.example", read: {resource: r, args: &L {agent_runs_per_day: 999}}}`,
		`{type: http, url: "https://e.example", read: {resource: r, args: *L}}`,
	} {
		if _, _, err := Effective([]byte(anchored), []Item{{Kind: "sources", Name: "a", YAML: y}}); err == nil || !strings.Contains(err.Error(), "anchor") {
			t.Errorf("%s: %v", y, err)
		}
	}
	if _, _, err := Effective([]byte(anchored), nil); err != nil { // the file's own anchors are fine
		t.Fatal(err)
	}
}

func TestOverlayFixedSectionsBackstop(t *testing.T) {
	a, _ := Parse([]byte("limits: {agent_runs_per_day: 5}\nunits: [a.service]\n"))
	for name, y := range map[string]string{
		"limits": "limits: {agent_runs_per_day: 999}\nunits: [a.service]\n",
		"units":  "limits: {agent_runs_per_day: 5}\nunits: [a.service, b.service]\n",
		"server": "server: {workers: 99}\nlimits: {agent_runs_per_day: 5}\nunits: [a.service]\n",
	} {
		b, _ := Parse([]byte(y))
		if sameFixedSections(a, b) == nil {
			t.Errorf("%s change not caught", name)
		}
	}
	if err := sameFixedSections(a, a); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayItemChecks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("server: { db: "+dir+"/s.db }\nsources:\n  m: { type: mcp, command: [srv], read: {resource: r}, poll: 1m }\n"), 0o600)
	sec := dir + "/secrets"
	for name, it := range map[string]Item{
		"command":       {Kind: "sources", Name: "n", YAML: "{type: mcp, command: [sh], read: {resource: r}}"},
		"changed cmd":   {Kind: "sources", Name: "m", YAML: "{type: mcp, command: [sh], read: {resource: r}}"},
		"allow_private": {Kind: "sources", Name: "n", YAML: "{type: http, url: 'https://e.example', allow_private: true}"},
		"env ref":       {Kind: "sources", Name: "n", YAML: "{type: webhook, secret: 'env:HOME', signature: github}"},
		"other file":    {Kind: "credentials", Name: "n", YAML: "{provider: claude, api_key: 'file:/etc/passwd'}"},
		"unclean path":  {Kind: "credentials", Name: "n", YAML: "{provider: claude, api_key: 'file:" + sec + "/../s.db'}"},
		"other item":    {Kind: "credentials", Name: "n", YAML: "{provider: claude, api_key: 'file:" + sec + "/credentials-x-api_key'}"},
		"no egress":     {Kind: "agents", Name: "n", YAML: "{kind: claude, egress: {enabled: false}}"},
	} {
		if _, _, err := LoadWithOverlay(path, []Item{it}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	os.MkdirAll(sec, 0o700)
	os.WriteFile(sec+"/credentials-n-api_key", []byte("k"), 0o600)
	for name, it := range map[string]Item{
		"same command":  {Kind: "sources", Name: "m", YAML: "{type: mcp, command: [srv], read: {resource: r}, poll: 2m}"},
		"own secret":    {Kind: "credentials", Name: "n", YAML: "{provider: claude, api_key: 'file:" + sec + "/credentials-n-api_key'}"},
		"allow a host":  {Kind: "agents", Name: "n", YAML: "{kind: claude, egress: {allow: [x.example.com]}}"},
		"tombstone any": {Kind: "sources", Name: "m", Deleted: true},
	} {
		if _, _, err := LoadWithOverlay(path, []Item{it}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckModelEndpoint(t *testing.T) {
	file, err := Parse([]byte("server: { models: {private_endpoints: ['10.0.0.9:11434']} }\n"))
	if err != nil {
		t.Fatal(err)
	}
	old := lookupHost
	defer func() { lookupHost = old }()
	lookupHost = func(_ context.Context, h string) ([]string, error) {
		switch h {
		case "pub.example":
			return []string{"93.184.216.34"}, nil
		case "lan.example":
			return []string{"93.184.216.34", "192.168.1.5"}, nil
		case "meta.example":
			return []string{"::ffff:169.254.169.254"}, nil
		case "10.0.0.9":
			return []string{"10.0.0.9"}, nil
		}
		return nil, errors.New("no such host")
	}
	cred := func(prov, u string) string { return "{provider: " + prov + ", url: '" + u + "'}" }
	for name, y := range map[string]string{
		"loopback":   cred("ollama", "http://127.0.0.1:11434"),
		"mixed":      cred("openai", "https://lan.example/v1"),
		"metadata":   cred("openai", "http://meta.example/v1"),
		"unresolved": cred("ollama", "http://nope.example:11434"),
		"unlisted":   cred("ollama", "http://10.0.0.9:11435"),
	} {
		if err := file.CheckModelEndpoint(y); err == nil || !strings.Contains(err.Error(), "server.models.private_endpoints") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, y := range map[string]string{
		"public": cred("openai", "https://pub.example/v1"),
		"listed": cred("ollama", "http://10.0.0.9:11434"),
		"claude": "{provider: claude}",
	} {
		if err := file.CheckModelEndpoint(y); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Loading never resolves names: a saved item whose host later turns private
// must not stop startup or validate.
func TestLoadDoesNoModelDNS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("server: { db: "+dir+"/s.db }\n"), 0o600)
	old := lookupHost
	defer func() { lookupHost = old }()
	lookupHost = func(context.Context, string) ([]string, error) {
		t.Error("DNS lookup during load")
		return nil, errors.New("x")
	}
	it := Item{Kind: "credentials", Name: "n", YAML: "{provider: openai, url: 'https://moved.example/v1'}"}
	if c, _, err := LoadWithOverlay(path, []Item{it}); err != nil || c.Credentials["n"] == nil {
		t.Fatalf("load: %v", err)
	}
}

// A key kept from siphon.yaml may not be pointed at another provider or URL.
func TestOverlayKeptSecretCannotMove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("server: { db: "+dir+"/s.db }\n"+
		"credentials:\n  oa: {provider: openai, url: 'https://api.example/v1', api_key: 'env:HOME'}\n"+
		"sources:\n  s: {type: http, url: 'https://good.example/x', poll: 1m, auth: {bearer: 'env:HOME'}, headers: {X-Key: 'env:HOME'}}\n"+
		"  h: {type: webhook, secret: 'env:HOME', signature: github}\n"), 0o600)
	const msg = "can't be moved"
	for name, it := range map[string]Item{
		"other url":      {Kind: "credentials", Name: "oa", YAML: "{provider: openai, url: 'https://evil.example/v1', api_key: 'env:HOME'}"},
		"other provider": {Kind: "credentials", Name: "oa", YAML: "{provider: ollama, url: 'https://api.example/v1', api_key: 'env:HOME'}"},
		"other path":     {Kind: "credentials", Name: "oa", YAML: "{provider: openai, url: 'https://api.example/other/v1', api_key: 'env:HOME'}"},
		"http downgrade": {Kind: "credentials", Name: "oa", YAML: "{provider: openai, url: 'http://api.example/v1', api_key: 'env:HOME'}"},
		"bearer":         {Kind: "sources", Name: "s", YAML: "{type: http, url: 'https://evil.example/x', poll: 1m, auth: {bearer: 'env:HOME'}}"},
		"header":         {Kind: "sources", Name: "s", YAML: "{type: http, url: 'https://evil.example/x', poll: 1m, headers: {X-Key: 'env:HOME'}}"},
	} {
		if _, _, err := LoadWithOverlay(path, []Item{it}); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, it := range map[string]Item{
		"same url":   {Kind: "credentials", Name: "oa", YAML: "{provider: openai, url: 'https://API.example/v1', api_key: 'env:HOME', concurrency: 2}"},
		"same src":   {Kind: "sources", Name: "s", YAML: "{type: http, url: 'https://good.example/x', poll: 2m, auth: {bearer: 'env:HOME'}}"},
		"no key url": {Kind: "credentials", Name: "oa", YAML: "{provider: openai, url: 'https://other.example/v1'}"},
		"webhook":    {Kind: "sources", Name: "h", YAML: "{type: webhook, secret: 'env:HOME', signature: github}"},
	} {
		if _, _, err := LoadWithOverlay(path, []Item{it}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOverlayEnvKeysFileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("server: { db: "+dir+"/s.db }\nsources:\n  m: { type: mcp, command: [srv], read: {tool: t}, env: {A_KEY: 'env:HOME', B_KEY: 'env:HOME'} }\n"), 0o600)
	sec := dir + "/secrets"
	item := func(env string) Item {
		return Item{Kind: "sources", Name: "m", YAML: "{type: mcp, command: [srv], read: {tool: t}, env: {" + env + "}}"}
	}
	for name, env := range map[string]string{
		"add":     "A_KEY: 'env:HOME', B_KEY: 'env:HOME', C_KEY: 'env:HOME'",
		"remove":  "A_KEY: 'env:HOME'",
		"rename":  "A_KEY: 'env:HOME', C_KEY: 'env:HOME'",
		"foreign": "A_KEY: 'file:/etc/passwd', B_KEY: 'env:HOME'",
	} {
		if _, _, err := LoadWithOverlay(path, []Item{item(env)}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	os.MkdirAll(sec, 0o700)
	os.WriteFile(sec+"/sources-m-env.A_KEY", []byte("v"), 0o600)
	if _, _, err := LoadWithOverlay(path, []Item{item("A_KEY: 'file:" + sec + "/sources-m-env.A_KEY', B_KEY: 'env:HOME'")}); err != nil {
		t.Errorf("rotate: %v", err)
	}
	if _, _, err := LoadWithOverlay(path, []Item{{Kind: "sources", Name: "n", YAML: "{type: mcp, command: [srv], read: {tool: t}, env: {A_KEY: x}}"}}); err == nil {
		t.Error("new portal stdio source accepted")
	}
}

func TestOverlayPackages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("server: { db: "+dir+"/s.db, mcp_packages: {github: {command: [gh], env: [GH_TOKEN, GH_HOST]}} }\n"), 0o600)
	sec := dir + "/secrets"
	os.MkdirAll(sec, 0o700)
	os.WriteFile(sec+"/sources-n-env.GH_TOKEN", []byte("v"), 0o600)
	src := func(extra string) Item {
		return Item{Kind: "sources", Name: "n", YAML: "{type: mcp, read: {tool: t}, " + extra + "}"}
	}
	for name, it := range map[string]Item{
		"unlisted":      src("package: other"),
		"with command":  src("package: github, command: [gh]"),
		"extra key":     src("package: github, env: {GH_TOKEN: x, EXTRA: x}"),
		"foreign ref":   src("package: github, env: {GH_TOKEN: 'file:/etc/passwd'}"),
		"server change": {Kind: "sources", Name: "n", YAML: "{type: mcp, command: [gh], read: {tool: t}}"},
	} {
		if _, _, err := LoadWithOverlay(path, []Item{it}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, it := range map[string]Item{
		"package":        src("package: github"),
		"package + env":  src("package: github, env: {GH_TOKEN: 'file:" + sec + "/sources-n-env.GH_TOKEN'}"),
		"package + keys": src("package: github, env: {GH_TOKEN: 'file:" + sec + "/sources-n-env.GH_TOKEN', GH_HOST: x}"),
	} {
		c, _, err := LoadWithOverlay(path, []Item{it})
		if err != nil || len(c.Sources["n"].Command) != 1 || c.Sources["n"].Command[0] != "gh" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The overlay cannot alter the package list (server stays file-only).
	b, _ := os.ReadFile(path)
	file, _ := Parse(b)
	if err := sameFixedSections(file, &Config{Server: Server{MCPPackages: map[string]MCPPackage{"github": {Command: []string{"evil"}}}}}); err == nil {
		t.Error("mcp_packages change accepted by sameFixedSections")
	}
}

// A portal-made source may use allow_private only for a listed service endpoint.
func TestOverlayServicePrivateEndpoint(t *testing.T) {
	file := []byte("server: { services: { private_endpoints: [\"gitlab.lan:443\"] } }\nsources:\n  s: { type: webhook, secret: env:X }\n")
	ok := []Item{{Kind: "sources", Name: "gl", YAML: "type: http\nurl: https://gitlab.lan/api/v4/user\nallow_private: true\n"}}
	f, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOverlay(f, ok, t.TempDir()); err != nil {
		t.Fatalf("listed: %v", err)
	}
	bad := []Item{{Kind: "sources", Name: "gl", YAML: "type: http\nurl: https://other.lan/api\nallow_private: true\n"}}
	if err := checkOverlay(f, bad, t.TempDir()); err == nil || !strings.Contains(err.Error(), "services.private_endpoints") {
		t.Fatalf("unlisted: %v", err)
	}
}

// A file secret kept on an item can't ride along when its package changes, or
// when a file `command` source becomes a `package` source.
func TestOverlayKeptSecretCannotMoveToPackage(t *testing.T) {
	file := []byte("server: { mcp_packages: { p: { command: [/bin/p], env: [FOO] }, q: { command: [/bin/q], env: [FOO] } } }\n" +
		"sources:\n" +
		"  cmd: { type: mcp, command: [/bin/c], env: { FOO: 'env:HOME' }, read: { tool: t } }\n" +
		"  pkg: { type: mcp, package: p, env: { FOO: 'env:HOME' }, read: { tool: t } }\n")
	f, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	for name, it := range map[string]Item{
		"command to package": {Kind: "sources", Name: "cmd", YAML: "{type: mcp, package: p, env: {FOO: 'env:HOME'}, read: {tool: t}}"},
		"package to package": {Kind: "sources", Name: "pkg", YAML: "{type: mcp, package: q, env: {FOO: 'env:HOME'}, read: {tool: t}}"},
	} {
		if err := checkOverlay(f, []Item{it}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "can't be moved") {
			t.Errorf("%s: %v", name, err)
		}
	}
	same := Item{Kind: "sources", Name: "pkg", YAML: "{type: mcp, package: p, env: {FOO: 'env:HOME'}, read: {tool: t}, poll: 2m}"}
	if err := checkOverlay(f, []Item{same}, t.TempDir()); err != nil {
		t.Errorf("same package: %v", err)
	}
}

// checkOverlay reports every problem at once, with the same messages.
func TestCheckOverlayCollectsAll(t *testing.T) {
	f, err := Parse([]byte("sources:\n  s: { type: webhook, secret: env:X }\n"))
	if err != nil {
		t.Fatal(err)
	}
	items := []Item{
		{Kind: "sources", Name: "a", YAML: "type: mcp\ncommand: [x]\nallow_private: true\n"},
		{Kind: "agents", Name: "b", YAML: "kind: claude\nprompt: p\negress: { enabled: false }\n"},
	}
	err = checkOverlay(f, items, t.TempDir())
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"command (stdio MCP) can only be set in siphon.yaml", "allow_private can only be set", "egress.enabled: false can only be set in siphon.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
