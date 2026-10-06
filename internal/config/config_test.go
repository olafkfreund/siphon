package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setenv(t *testing.T) {
	t.Setenv("AGW_TOKEN", "tok-secret")
	t.Setenv("AGW_FACTORY", "fac-secret")
	t.Setenv("AGW_GH", "gh-secret")
}

func TestFullLoads(t *testing.T) {
	setenv(t)
	c, err := Load("testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Limits.HTTPMaxBody != 1<<20 || time.Duration(c.Sources["factory"].Poll) != time.Minute {
		t.Fatalf("limits/poll: %+v %+v", c.Limits, c.Sources["factory"])
	}
	if !*c.Agents["triage-task"].Approve {
		t.Fatal("agent approve should default true")
	}
	if got := strings.Join(c.Secrets(), ","); got != "tok-secret,fac-secret,gh-secret" {
		t.Fatalf("secrets: %s", got)
	}
}

func TestBadListsAll(t *testing.T) {
	c, err := Load("testdata/bad.yaml")
	if err != nil {
		t.Fatal(err)
	}
	msg := c.Validate().Error()
	for _, want := range []string{
		"inline secret", "unknown source \"nope\"", "bad expression", "cooldown is mandatory",
		"not in the units allowlist", "unknown routine", "mcp references unknown source",
		"units: \"{{.x}}.service\" must not be templated", "on: each requires id",
		"type must be mcp, http or webhook", "signature must be github or sha256",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	if len(c.Warnings()) != 1 {
		t.Errorf("want sandbox warning, got %v", c.Warnings())
	}
}

func TestValidationCases(t *testing.T) {
	setenv(t)
	base := `
sources:
  s: { type: http, url: "http://x" }
agents:
  a: { prompt: hi }
routines:
  r: { steps: [{ id: x, agent: a }] }
units: [u.service]
`
	cases := []struct{ name, rules, want string }{
		{"ok", `[{name: r, source: s, when: "true", action: {cmd: [true]}}]`, ""},
		{"agent needs cooldown", `[{name: r, source: s, when: "true", action: {agent: a}}]`, "cooldown is mandatory"},
		{"routine with agent needs cooldown", `[{name: r, source: s, when: "true", action: {routine: r}}]`, "cooldown is mandatory"},
		{"agent with cooldown ok", `[{name: r, source: s, when: "true", cooldown: 1m, action: {agent: a}}]`, ""},
		{"unknown source", `[{name: r, source: z, when: "true", action: {cmd: [true]}}]`, "unknown source"},
		{"bad when", `[{name: r, source: s, when: "1 +", action: {cmd: [true]}}]`, "bad expression"},
		{"bad id", `[{name: r, source: s, when: "true", id: "(", on: each, action: {cmd: [true]}}]`, "bad expression"},
		{"bad for_each", `[{name: r, source: s, when: "true", for_each: "(", action: {cmd: [true]}}]`, "bad expression"},
		{"unit not allowed", `[{name: r, source: s, when: "true", action: {unit: x.service}}]`, "allowlist"},
		{"unit ok", `[{name: r, source: s, when: "true", action: {unit: u.service}}]`, ""},
		{"templated unit", `[{name: r, source: s, when: "true", action: {unit: "{{.a}}"}}]`, "must not be templated"},
		{"no action", `[{name: r, source: s, when: "true", action: {}}]`, "exactly one"},
		{"two actions", `[{name: r, source: s, when: "true", action: {cmd: [a], unit: u.service}}]`, "exactly one"},
		{"bad template", `[{name: r, source: s, when: "true", action: {cmd: ["{{"]}}]`, "bad template"},
		{"templated cmd[0]", `[{name: r, source: s, when: "true", action: {cmd: ["{{.a}}", x]}}]`, "cmd[0] must not be templated"},
		{"bad on", `[{name: r, source: s, when: "true", on: sometimes, action: {cmd: [a]}}]`, "on must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(base + "rules: " + tc.rules))
			if err != nil {
				t.Fatal(err)
			}
			err = c.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestInlineSecretAndMissingEnv(t *testing.T) {
	c, _ := Parse([]byte("server: {token: plaintext}\n"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "inline secret") {
		t.Fatalf("got %v", err)
	}
	c, _ = Parse([]byte("server: {token: \"env:AGW_NOPE_UNSET\"}\n"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "unset") {
		t.Fatalf("got %v", err)
	}
}

func TestFileSecretAndUnknownKey(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(f, []byte("from-file\n"), 0o600)
	c, _ := Parse([]byte("server: {token: \"file:" + f + "\"}\n"))
	if c.Server.Token.Value != "from-file" || c.Validate() != nil {
		t.Fatalf("%+v %v", c.Server.Token, c.Validate())
	}
	if _, err := Parse([]byte("bogus: 1\n")); err == nil {
		t.Fatal("unknown key should fail")
	}
	if _, err := Parse([]byte("limits: {http_timeout: soon}\n")); err == nil {
		t.Fatal("bad duration should fail")
	}
}

func TestRoutineAndRunnerTemplatedArgv0(t *testing.T) {
	c, _ := Parse([]byte(`
agents: { a: { runner: ["{{.x}}", -p] } }
routines: { r: { steps: [{ id: s, cmd: ["{{.y}}"] }] } }
`))
	msg := c.Validate().Error()
	for _, w := range []string{"runner[0] must not be templated", "cmd[0] must not be templated"} {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in %s", w, msg)
		}
	}
}

func TestHeaders(t *testing.T) {
	t.Setenv("AGW_H", "hdr-secret")
	c, _ := Parse([]byte(`
sources:
  s:
    type: http
    url: http://x
    headers: { Accept: application/json, X-Api-Key: "env:AGW_H" }
`))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.Secrets(); len(got) != 1 || got[0] != "hdr-secret" {
		t.Fatalf("secrets: %v", got)
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Auth-Token", "X-Api-Key", "My-Secret"} {
		c, _ = Parse([]byte("sources: {s: {type: http, url: http://x, headers: {" + h + ": literal}}}"))
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "inline secret") {
			t.Errorf("%s: want inline secret error, got %v", h, err)
		}
	}
}

func TestEmptySecretIsError(t *testing.T) {
	t.Setenv("AGW_EMPTY", "")
	f := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(f, []byte("\n"), 0o600)
	for _, ref := range []string{"env:AGW_EMPTY", "file:" + f} {
		c, _ := Parse([]byte("server: {token: \"" + ref + "\"}\n"))
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want error", ref)
		}
	}
}

func TestBearerCleartextWarning(t *testing.T) {
	t.Setenv("AGW_B", "b")
	for url, want := range map[string]int{"http://p510:8090/mcp": 1, "http://localhost:1/m": 0, "http://127.0.0.1/m": 0, "https://x/m": 0} {
		c, _ := Parse([]byte("sources: {s: {type: mcp, url: \"" + url + "\", read: {resource: r://x}, auth: {bearer: env:AGW_B}}}"))
		if got := len(c.Warnings()); got != want {
			t.Errorf("%s: warnings=%d want %d", url, got, want)
		}
	}
}
