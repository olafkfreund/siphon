package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setenv(t *testing.T) {
	t.Setenv("AGW_TOKEN", "tok-secret-0123456789abcdef0123456789")
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
	if got := strings.Join(c.Secrets(), ","); got != "tok-secret-0123456789abcdef0123456789,fac-secret,gh-secret" {
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
	if !hasWarning(c, "sandbox") {
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
	if c.Server.Token.Value != "from-file" {
		t.Fatalf("%+v", c.Server.Token)
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
		if got := hasWarning(c, "bearer"); got != (want == 1) {
			t.Errorf("%s: bearer warning=%v want %v", url, got, want == 1)
		}
	}
}

func TestDurationsAndAgentMCP(t *testing.T) {
	t.Setenv("AGW_X", "x")
	c, _ := Parse([]byte(`
sources:
  neg: { type: http, url: "http://x", poll: -1m }
  web: { type: webhook, secret: env:AGW_X, signature: github }
  h:   { type: http, url: "http://x" }
agents:
  a: { mcp: [h], timeout: -1s }
  b: { prompt: hi }
routines: { r: { steps: [{ id: s, cmd: [true], timeout: -1s }] } }
rules:
  - { name: r, source: h, when: "true", cooldown: -1m, action: { cmd: [true] } }
`))
	if c.Agents["b"].Timeout != Duration(10*time.Minute) {
		t.Fatalf("agent timeout default: %v", c.Agents["b"].Timeout)
	}
	if c.Sources["web"].Poll != 0 || c.Sources["h"].Poll != Duration(time.Minute) {
		t.Fatal("poll defaults")
	}
	msg := c.Validate().Error()
	for _, w := range []string{
		"sources.neg: poll must be > 0", "has type http, want mcp", "agents.a: timeout must not be negative",
		"steps[0]: timeout must not be negative", "repeat and cooldown must not be negative",
	} {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in %s", w, msg)
		}
	}
	if strings.Contains(msg, "sources.web") {
		t.Error("webhook needs no poll")
	}
}

func TestLoadMakesDBRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	for in, want := range map[string]string{
		"state.db":      filepath.Join(dir, "state.db"),
		"/abs/state.db": "/abs/state.db",
		"":              filepath.Join(dir, "agentgw.db"), // default is relative too
	} {
		body := "server: {}\n"
		if in != "" {
			body = "server: {db: " + in + "}\n"
		}
		f := filepath.Join(dir, "c.yaml")
		os.WriteFile(f, []byte(body), 0o600)
		c, err := Load(f)
		if err != nil || c.Server.DB != want {
			t.Errorf("%q: got %q (%v) want %q", in, c.Server.DB, err, want)
		}
	}
}

func hasWarning(c *Config, sub string) bool {
	for _, w := range c.Warnings() {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestTokenLengthAndListenWarning(t *testing.T) {
	t.Setenv("AGW_SHORT", "too-short")
	t.Setenv("AGW_LONG", strings.Repeat("x", 32))
	c, _ := Parse([]byte("server: {token: env:AGW_SHORT}\n"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("short token: %v", err)
	}
	c, _ = Parse([]byte("server: {token: env:AGW_LONG}\n"))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for listen, warn := range map[string]bool{":8080": true, "0.0.0.0:80": true, "127.0.0.1:8080": false, "[::1]:8080": false, "localhost:80": false} {
		c, _ := Parse([]byte("server: {listen: \"" + listen + "\"}\n"))
		if got := hasWarning(c, "server.listen"); got != warn {
			t.Errorf("%s: warn=%v want %v", listen, got, warn)
		}
	}
}

func TestWebhookIDAndTimestamp(t *testing.T) {
	t.Setenv("AGW_W", "w")
	for _, tc := range []struct{ extra, want string }{
		{"", ""},
		{", id: header.X-GitHub-Delivery", ""},
		{", id: body.id", "id must look like header."},
		{", id: \"header.a b\"", "id must look like header."},
		{", timestamp_header: X-T", "timestamp_header is not allowed"},
	} {
		c, _ := Parse([]byte("sources: {w: {type: webhook, secret: env:AGW_W, signature: github" + tc.extra + "}}"))
		err := c.Validate()
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q: %v", tc.extra, err)
		}
	}
	c, _ := Parse([]byte("sources: {w: {type: webhook, secret: env:AGW_W, signature: sha256, signature_header: X-S, timestamp_header: X-T}}"))
	if err := c.Validate(); err != nil {
		t.Fatalf("sha256 preset may use a timestamp: %v", err)
	}
}

func TestRetryValidation(t *testing.T) {
	for retry, want := range map[string]string{
		"{attempts: 3, base: 5s, factor: 2}": "",
		"{attempts: 11}":                     "retry.attempts must be 0..10",
		"{attempts: -1}":                     "retry.attempts must be 0..10",
		"{attempts: 2, factor: 0.5}":         "retry.factor >= 1",
		"{attempts: 2, base: -1s}":           "retry.factor >= 1",
	} {
		c, err := Parse([]byte("routines: {r: {steps: [{id: s, cmd: [true], retry: " + retry + "}]}}"))
		if err != nil {
			t.Fatal(err)
		}
		err = c.Validate()
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", retry, err)
		}
	}
}

func TestUnitNamesAndAgentRetry(t *testing.T) {
	for u, ok := range map[string]bool{"nix-gc.service": true, "backup@home.timer": true, "x.target": true,
		"nix-gc": false, "a b.service": false, "-x.service": true, "evil;rm.service": false, "x.socket": false} {
		c, _ := Parse([]byte("units: [\"" + u + "\"]"))
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("%q: %v", u, err)
		}
	}
	c, _ := Parse([]byte("agents: {a: {prompt: hi}}\nroutines: {r: {steps: [{id: s, agent: a, retry: {attempts: 2}}]}}"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "retry is not allowed on agent steps") {
		t.Fatalf("%v", err)
	}
}

func TestAgentCredentials(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	os.WriteFile(key, []byte("sk-x\n"), 0o600)
	cases := []struct {
		name, yaml, wantErr, wantCred string
		warns                         []string
	}{
		{"default single subscription", "credentials: {cx: {provider: codex}}\nagents: {a: {kind: codex}}", "", "cx", nil},
		{"explicit wins", "credentials: {c1: {provider: claude}, c2: {provider: claude}}\nagents: {a: {credential: c2}}", "", "c2", nil},
		{"ambiguous", "credentials: {c1: {provider: claude}, c2: {provider: claude}}\nagents: {a: {}}", "credential is required", "", nil},
		{"none for codex", "agents: {a: {kind: codex}}", "credential is required", "", nil},
		{"api key credential is no default", "credentials: {k: {provider: claude, api_key: \"file:" + key + "\"}}\nagents: {a: {}}", "credential is required", "", nil},
		{"kind mismatch", "credentials: {cx: {provider: codex}}\nagents: {a: {credential: cx}}", "is for codex, agent kind is claude", "cx", nil},
		{"unknown credential", "credentials: {cx: {provider: codex}}\nagents: {a: {credential: nope}}", "unknown credential", "nope", nil},
		{"bad provider", "credentials: {x: {provider: gpt}}", "provider must be", "", nil},
		{"bad concurrency", "credentials: {x: {provider: codex, concurrency: -1}}", "concurrency must be >= 1", "", nil},
		{"bad kind", "agents: {a: {kind: foo}}", "kind must be", "", nil},
		{"runner alias", "agents: {a: {runner: [/bin/c, --bare, -p]}}", "", "", []string{"runner is deprecated", "arguments after the binary are ignored"}},
		{"runner alias, one arg", "agents: {a: {runner: [/bin/c]}}", "", "", []string{"runner is deprecated"}},
		{"runner with codex", "credentials: {cx: {provider: codex}}\nagents: {a: {kind: codex, runner: [x]}}", "runner implies kind claude", "cx", nil},
		{"api_key_file implicit", "agents: {a: {kind: codex, api_key_file: " + key + "}}", "", "_apikey_a", nil},
		{"codex warnings", "credentials: {cx: {provider: codex}}\nagents: {a: {kind: codex, max_turns: 3, max_budget_usd: 1}}", "", "cx",
			[]string{"shell tools", "max_turns not enforced", "max_budget_usd not enforced"}},
		{"agy warnings", "credentials: {g: {provider: agy}}\nagents: {a: {kind: agy, mcp: [m]}}\nsources: {m: {type: mcp, url: http://127.0.0.1/mcp, read: {tool: t}}}", "", "g",
			[]string{"shell tools", "tool allowlist not enforced"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			verr := c.Validate()
			if tc.wantErr == "" && verr != nil {
				t.Fatalf("unexpected: %v", verr)
			}
			if tc.wantErr != "" && (verr == nil || !strings.Contains(verr.Error(), tc.wantErr)) {
				t.Fatalf("want %q, got %v", tc.wantErr, verr)
			}
			if got, _ := c.AgentCredential("a"); tc.wantCred != "" && got != tc.wantCred {
				t.Fatalf("credential %q, want %q", got, tc.wantCred)
			}
			for _, w := range tc.warns {
				if !hasWarning(c, w) {
					t.Errorf("missing warning %q in %v", w, c.Warnings())
				}
			}
		})
	}
}

func TestAliasMappingAndWarningsAbsent(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	os.WriteFile(key, []byte("sk-x\n"), 0o600)
	c, _ := Parse([]byte("agents: {a: {runner: [/bin/c, -p], api_key_file: " + key + "}}"))
	a := c.Agents["a"]
	if a.Kind != "claude" || a.Command != "/bin/c" {
		t.Fatalf("alias: %+v", a)
	}
	name, cr := c.AgentCredential("a")
	if name != "_apikey_a" || cr.Provider != "claude" || cr.APIKey.Value != "sk-x" || cr.Concurrency != 1 {
		t.Fatalf("implicit: %s %+v", name, cr)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// claude with unset limits and a codex agent without them: no kind warnings
	c, _ = Parse([]byte("credentials: {cx: {provider: codex}}\nagents: {a: {kind: codex}}"))
	if hasWarning(c, "max_turns") || hasWarning(c, "max_budget") {
		t.Fatalf("spurious: %v", c.Warnings())
	}
}

func TestLegacyExemptionAndNames(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	os.WriteFile(key, []byte("sk\n"), 0o600)
	c, _ := Parse([]byte("agents: {a: {api_key_file: " + key + "}, b: {}}"))
	if err := c.Validate(); err != nil {
		t.Fatalf("legacy mix must validate: %v", err)
	}
	for yaml, want := range map[string]string{
		"credentials: {Bad_Name: {provider: claude}}":                                            "name must match",
		"credentials: {_apikey_x: {provider: claude}}":                                           "reserved",
		"credentials: {_apikey_a: {provider: claude}}\nagents: {a: {api_key_file: " + key + "}}": "collides",
	} {
		c, _ := Parse([]byte(yaml))
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", yaml, want, err)
		}
	}
}

func TestFollowupValidation(t *testing.T) {
	cases := map[string]string{
		"sources: {s: {type: http, url: \"http://u:p@h/x\"}}":                                                                             "must not contain credentials",
		"sources: {s: {type: mcp, url: \"http://u@h/x\", read: {tool: t}}}":                                                               "must not contain credentials",
		"sources: {w: {type: webhook, secret: \"env:AGW_FACTORY\", signature: sha256, signature_header: X-Sig, timestamp_header: x-sig}}": "must differ",
		"server: {workers: 1}\n---\nserver: {workers: 2}":                                                                                 "multiple YAML documents",
	}
	setenv(t)
	for y, want := range cases {
		c, err := Parse([]byte(y))
		if err == nil {
			err = c.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", y, want, err)
		}
	}
	if _, err := Parse([]byte("server: {workers: 1}\n")); err != nil {
		t.Fatal(err)
	}
	// agy warns about the allowlist whenever it has mcp servers, and only then
	c, _ := Parse([]byte("credentials: {g: {provider: agy}}\nagents: {a: {kind: agy, allowed_tools: [x]}}"))
	if hasWarning(c, "allowlist") {
		t.Fatal("no mcp: no allowlist warning")
	}
}

func hostsOf(a []HostPort) string {
	var s []string
	for _, h := range a {
		s = append(s, h.String())
	}
	return strings.Join(s, ",")
}

func TestEgressLists(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	os.WriteFile(key, []byte("sk\n"), 0o600)
	y := `
server: {egress: {allow: [global.example.com]}}
sources:
  m1: {type: mcp, url: "https://mcp.example.com/x", read: {tool: t}}
  m2: {type: mcp, url: "http://10.0.0.5:9000/mcp", allow_private: true, read: {tool: t}}
  m3: {type: mcp, command: [srv], read: {tool: t}}
credentials:
  cx: {provider: codex}
  cc: {provider: claude}
  ck: {provider: codex, api_key: "file:` + key + `"}
agents:
  sub:  {kind: codex, credential: cx, mcp: [m1, m2, m3], egress: {allow: ["api.github.com", "*.corp.example:8443"]}}
  key:  {kind: codex, credential: ck}
  off:  {kind: codex, credential: cx, egress: {enabled: false}}
  cl:   {}
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	got, on := c.AgentEgress("sub")
	want := "chatgpt.com:443,auth.openai.com:443,mcp.example.com:443,10.0.0.5:9000 (allow_private),api.github.com:443,*.corp.example:8443,global.example.com:443"
	if !on || hostsOf(got) != want {
		t.Fatalf("sub: %v\n%s", on, hostsOf(got))
	}
	if got, _ := c.AgentEgress("key"); hostsOf(got) != "api.openai.com:443,global.example.com:443" {
		t.Fatalf("key: %s", hostsOf(got))
	}
	if got, on := c.AgentEgress("off"); on || got != nil {
		t.Fatal("off must be disabled")
	}
	if got, on := c.AgentEgress("cl"); !on || !strings.HasPrefix(hostsOf(got), "api.anthropic.com:443,platform.claude.com:443") {
		t.Fatalf("legacy claude: %s", hostsOf(got))
	}
	for kind, wantFirst := range map[string]string{"claude": "api.anthropic.com:443", "agy": "oauth2.googleapis.com:443"} {
		c, _ := Parse([]byte("credentials: {x: {provider: " + kind + "}}\nagents: {a: {kind: " + kind + "}}"))
		if got, _ := c.AgentEgress("a"); !strings.HasPrefix(hostsOf(got), wantFirst) {
			t.Errorf("%s: %s", kind, hostsOf(got))
		}
	}
	if c.Server.Egress.Listen != "127.77.0.1:3128" {
		t.Fatal("default listen")
	}
}

func TestRuleEgress(t *testing.T) {
	c, _ := Parse([]byte("server: {egress: {allow: [g.example.com]}}"))
	if _, on := c.RuleEgress(Rule{}); on {
		t.Fatal("cmd egress is opt-in")
	}
	got, on := c.RuleEgress(Rule{Egress: EgressRule{Enabled: true, Allow: []string{"h.example.com:8080"}}})
	if !on || hostsOf(got) != "h.example.com:8080,g.example.com:443" {
		t.Fatalf("%v %s", on, hostsOf(got))
	}
	c, _ = Parse([]byte("server: {egress: {cmd_default: true}}"))
	if got, on := c.RuleEgress(Rule{}); !on || len(got) != 0 {
		t.Fatal("cmd_default must enable with an empty list")
	}
}

func TestEgressValidation(t *testing.T) {
	for y, want := range map[string]string{
		"server: {egress: {listen: \"0.0.0.0:3128\"}}":                                                            "loopback",
		"server: {egress: {listen: \"example.com:3128\"}}":                                                        "loopback",
		"server: {egress: {listen: \"[::1]:3128\"}}":                                                              "",
		"server: {egress: {allow: [\"bad host\"]}}":                                                               "egress.allow",
		"server: {egress: {allow: [\"a.com:0\"]}}":                                                                "port must be",
		"server: {egress: {allow: [\"a.com:70000\"]}}":                                                            "port must be",
		"server: {egress: {allow: [\"*.a.com\", \"b.com:8443\"]}}":                                                "",
		"agents: {a: {egress: {allow: [\"http://x\"]}}}":                                                          "agents.a.egress.allow",
		"rules: [{name: r, source: agent-result, when: \"true\", action: {cmd: [x]}, egress: {allow: [\"-x\"]}}]": "rules[0].egress.allow",
	} {
		c, err := Parse([]byte(y))
		if err == nil {
			err = c.Validate()
		}
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: want %q, got %v", y, want, err)
		}
	}
	c, _ := Parse([]byte("server: {sandbox: none}\ncredentials: {x: {provider: claude}}\nagents: {a: {}, b: {egress: {enabled: false}}}"))
	n := 0
	for _, w := range c.Warnings() {
		if strings.Contains(w, "egress allowlists are not enforced with sandbox: none") {
			n++
			if !strings.Contains(w, "agent a") {
				t.Errorf("wrong agent: %s", w)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want one warning, got %d", n)
	}
}
