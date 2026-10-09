package config

import (
	"os"
	"path/filepath"
	"slices"
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
		"type must be mcp, http, webhook or schedule", "signature must be github, sha256",
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

func TestHeaderAndEnvCleartextWarning(t *testing.T) {
	t.Setenv("AGW_B", "b")
	for url, want := range map[string]bool{"http://gitlab.lan/api": true, "http://localhost:1/api": false, "https://gitlab.lan/api": false} {
		c, _ := Parse([]byte("sources: {s: {type: http, url: \"" + url + "\", poll: 1m, headers: {PRIVATE-TOKEN: env:AGW_B}}}"))
		if got := hasWarning(c, "header PRIVATE-TOKEN"); got != want {
			t.Errorf("%s: header warning=%v want %v", url, got, want)
		}
	}
	c, _ := Parse([]byte("server: {mcp_packages: {p: {command: [/bin/p], env: [K]}}}\nsources: {s: {type: mcp, package: p, url: \"\", env: {K: env:AGW_B}, read: {tool: t}}}"))
	c.Sources["s"].URL = "http://mcp.lan/x" // env on a plain http URL, non-loopback
	if !hasWarning(c, "env K") {
		t.Errorf("no env warning: %v", c.Warnings())
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
		"":              filepath.Join(dir, "siphon.db"), // default is relative too
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
	for _, tc := range []struct{ extra, want string }{
		{"signature: token, token_header: X-Gitlab-Token", ""},
		{"signature: token", "needs token_header"},
		{"signature: standard-webhooks", ""},
		{"signature: standard-webhooks, signature_header: X-S", "not allowed with signature standard-webhooks"},
		{"signature: standard-webhooks, timestamp_header: X-T", "not allowed with signature standard-webhooks"},
	} {
		c, _ := Parse([]byte("sources: {w: {type: webhook, secret: env:AGW_W, " + tc.extra + "}}"))
		err := c.Validate()
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q: %v", tc.extra, err)
		}
		if got := hasWarning(c, "weaker than an HMAC"); got != strings.HasPrefix(tc.extra, "signature: token") {
			t.Errorf("%q: token warning = %v", tc.extra, got)
		}
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
	got, on := c.AgentEgress(c.Agents["sub"])
	want := "chatgpt.com:443,auth.openai.com:443,mcp.example.com:443,10.0.0.5:9000 (allow_private) (no_link_local),api.github.com:443,*.corp.example:8443,global.example.com:443"
	if !on || hostsOf(got) != want {
		t.Fatalf("sub: %v\n%s", on, hostsOf(got))
	}
	if got, _ := c.AgentEgress(c.Agents["key"]); hostsOf(got) != "api.openai.com:443,global.example.com:443" {
		t.Fatalf("key: %s", hostsOf(got))
	}
	if got, on := c.AgentEgress(c.Agents["off"]); on || got != nil {
		t.Fatal("off must be disabled")
	}
	if got, on := c.AgentEgress(c.Agents["cl"]); !on || !strings.HasPrefix(hostsOf(got), "api.anthropic.com:443,platform.claude.com:443") {
		t.Fatalf("legacy claude: %s", hostsOf(got))
	}
	for kind, wantFirst := range map[string]string{"claude": "api.anthropic.com:443", "agy": "oauth2.googleapis.com:443"} {
		c, _ := Parse([]byte("credentials: {x: {provider: " + kind + "}}\nagents: {a: {kind: " + kind + "}}"))
		if got, _ := c.AgentEgress(c.Agents["a"]); !strings.HasPrefix(hostsOf(got), wantFirst) {
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
		"server: {egress: {listen: \"[::1]:3128\"}}":                                                              "IPv4 loopback",
		"server: {egress: {socket: rel/egress.sock}}":                                                             "egress.socket",
		"server: {egress: {socket: /run/siphon/egress.sock}}":                                                     "",
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

// An agent missing from config, or a snapshot from before egress existed,
// must be restricted, never open.
func TestAgentEgressFailsClosed(t *testing.T) {
	c, _ := Parse([]byte("agents: {a: {kind: codex}}"))
	if _, on := c.AgentEgress(nil); !on {
		t.Fatal("nil agent ran unrestricted")
	}
	if got, on := c.AgentEgress(&Agent{Kind: "codex"}); !on || hostsOf(got) != "chatgpt.com:443,auth.openai.com:443" {
		t.Fatalf("old snapshot: %v %s", on, hostsOf(got))
	}
}

func TestModelValidation(t *testing.T) {
	t.Setenv("K", "sk-test")
	const ok = "credentials: {o: {provider: ollama, url: 'http://h:11434'}}\nagents: {m: {kind: model, model: q, credential: o}}\n"
	for name, tc := range map[string]struct{ y, want string }{
		"ok":             {ok, ""},
		"openai key":     {"credentials: {o: {provider: openai, url: 'https://api.x/v1', preset: groq, api_key: 'env:K'}}\nagents: {m: {kind: model, model: q, credential: o}}\n", ""},
		"no url":         {"credentials: {o: {provider: ollama}}\n", "url:"},
		"bad scheme":     {"credentials: {o: {provider: openai, url: 'ftp://h'}}\n", "url:"},
		"userinfo":       {"credentials: {o: {provider: openai, url: 'http://u:p@h'}}\n", "url:"},
		"ollama key":     {"credentials: {o: {provider: ollama, url: 'http://h', api_key: 'env:K'}}\n", "not allowed for ollama"},
		"url on claude":  {"credentials: {o: {provider: claude, url: 'http://h'}}\n", "url is only for"},
		"no model":       {strings.Replace(ok, "model: q, ", "", 1), "model is required"},
		"wrong cred":     {"credentials: {c: {provider: claude}}\nagents: {m: {kind: model, model: q, credential: c}}\n", "needs an ollama or openai"},
		"no cred":        {"agents: {m: {kind: model, model: q}}\n", "credential is required"},
		"bad endpoint":   {"server: {models: {private_endpoints: ['h']}}\n", "private_endpoints"},
		"wild endpoint":  {"server: {models: {private_endpoints: ['*.x:1']}}\n", "private_endpoints"},
		"query":          {"credentials: {o: {provider: ollama, url: 'http://h:11434/?x'}}\n", "must not contain ?"},
		"fragment":       {"credentials: {o: {provider: ollama, url: 'http://h:11434/api/pull#'}}\n", "must not contain ?"},
		"dotdot":         {"credentials: {o: {provider: openai, url: 'http://h/v1/../api'}}\n", "path segments"},
		"ollama path":    {"credentials: {o: {provider: ollama, url: 'http://h:11434/api/pull'}}\n", "ollama takes no path"},
		"openai path":    {"credentials: {o: {provider: openai, url: 'http://h/api/pull'}}\n", "end in /v1"},
		"openai V1":      {"credentials: {o: {provider: openai, url: 'http://h/V1'}}\n", "end in /v1"},
		"openai /v1/":    {"credentials: {o: {provider: openai, url: 'http://h/openai/v1/'}}\n", ""},
		"dunder mcp":     {"sources: {a__b: {type: mcp, url: 'http://m/x', read: {tool: t}}}\ncredentials: {o: {provider: ollama, url: 'http://h:11434'}}\nagents: {m: {kind: model, model: q, credential: o, mcp: [a__b]}}\n", "must not contain __"},
		"good endpoints": {"server: {models: {private_endpoints: ['127.0.0.1:11434', '[::1]:80']}}\n", ""},
	} {
		c, err := Parse([]byte(tc.y))
		if err == nil {
			err = c.Validate()
		}
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestModelEgress(t *testing.T) {
	c, err := Parse([]byte(`
server: {models: {private_endpoints: ['10.0.0.5:11434']}}
credentials:
  pub:  {provider: openai, url: 'https://api.groq.com/openai/v1'}
  lan:  {provider: ollama, url: 'http://10.0.0.5:11434'}
  bad:  {provider: ollama, url: 'http://10.0.0.6:11434'}
agents:
  a: {kind: model, model: m, credential: pub}
  b: {kind: model, model: m, credential: lan}
  c: {kind: model, model: m, credential: bad}
`))
	if err != nil {
		t.Fatal(err)
	}
	for a, want := range map[string]string{
		"a": "api.groq.com:443",
		"b": "10.0.0.5:11434 (allow_private) (no_link_local)",
		"c": "10.0.0.6:11434", // unlisted: public mode, the guard refuses it
	} {
		if got, _ := c.AgentEgress(c.Agents[a]); hostsOf(got) != want {
			t.Errorf("%s: %s, want %s", a, hostsOf(got), want)
		}
	}
}

// Plan step 1: container detection seam, sandbox default and the systemd error.
func TestContainerSandbox(t *testing.T) {
	old := InContainer
	t.Cleanup(func() { InContainer = old })
	for _, in := range []bool{false, true} {
		InContainer = func() bool { return in }
		c, err := Parse([]byte("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]string{false: "systemd", true: "none"}[in]; c.Server.Sandbox != want {
			t.Errorf("container=%v: sandbox %q, want %q", in, c.Server.Sandbox, want)
		}
		c, err = Parse([]byte("server: {sandbox: systemd}"))
		if err == nil {
			err = c.Validate()
		}
		if got := err != nil && strings.Contains(err.Error(), "systemd sandbox is not available inside a container; use the NixOS module or the microVM for isolation"); got != in {
			t.Errorf("container=%v: systemd error = %v (%v)", in, got, err)
		}
	}
}

func TestInContainerEnv(t *testing.T) {
	t.Setenv("container", "podman")
	if !InContainer() {
		t.Fatal("container env not detected")
	}
}

func TestSourceEnvValidation(t *testing.T) {
	t.Setenv("AGW_E", "secret-value")
	for env, want := range map[string]string{
		"{type: mcp, command: [s], read: {tool: t}, env: {GITHUB_TOKEN: 'env:AGW_E'}}":      "",
		"{type: mcp, command: [s], read: {tool: t}, env: {_X1: 'env:AGW_E'}}":               "",
		"{type: mcp, command: [s], read: {tool: t}, env: {lower: 'env:AGW_E'}}":             "must match",
		"{type: mcp, command: [s], read: {tool: t}, env: {'1A': 'env:AGW_E'}}":              "must match",
		"{type: mcp, command: [s], read: {tool: t}, env: {A: literal}}":                     "inline secret",
		"{type: mcp, url: 'https://e.example/mcp', read: {tool: t}, env: {A: 'env:AGW_E'}}": "env needs a stdio",
		"{type: http, url: 'https://e.example', env: {A: 'env:AGW_E'}}":                     "env needs a stdio",
	} {
		c, err := Parse([]byte("sources: {s: " + env + "}"))
		if err == nil {
			err = c.Validate()
		}
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", env, err)
		}
	}
	for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "PATH", "HOME", "NODE_OPTIONS", "PYTHONPATH", "PYTHONSTARTUP", "BASH_ENV", "ENV", "PERL5LIB", "RUBYOPT", "JAVA_TOOL_OPTIONS", "SSL_CERT_FILE", "GIT_SSH_COMMAND", "HTTPS_PROXY", "ALL_PROXY"} {
		c, _ := Parse([]byte("sources: {s: {type: mcp, command: [s], read: {tool: t}, env: {" + name + ": 'env:AGW_E'}}}"))
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Masking: the resolved value is in Secrets().
	c, _ := Parse([]byte("sources: {s: {type: mcp, command: [s], read: {tool: t}, env: {GITHUB_TOKEN: 'env:AGW_E'}}}"))
	if !slices.Contains(c.Secrets(), "secret-value") {
		t.Errorf("env value not masked: %v", c.Secrets())
	}
}

const pkgServer = "server: {mcp_packages: {github: {command: [github-mcp-server, stdio], env: [GITHUB_PERSONAL_ACCESS_TOKEN], hosts: [api.github.com, ghe.example:8443]}}}\n"

func TestSourcePackage(t *testing.T) {
	t.Setenv("AGW_E", "v")
	for src, want := range map[string]string{
		"{type: mcp, package: github, read: {tool: t}}":                                                   "",
		"{type: mcp, package: github, read: {tool: t}, env: {GITHUB_PERSONAL_ACCESS_TOKEN: 'env:AGW_E'}}": "",
		"{type: mcp, package: github, read: {tool: t}, env: {OTHER: 'env:AGW_E'}}":                        "not one of package",
		"{type: mcp, package: nope, read: {tool: t}}":                                                     "not in server.mcp_packages",
		"{type: mcp, package: github, command: [sh], read: {tool: t}}":                                    "package excludes",
		"{type: mcp, package: github, command: [github-mcp-server, stdio], read: {tool: t}}":              "package excludes",
		"{type: mcp, package: github, url: 'https://e.example/mcp', read: {tool: t}}":                     "set exactly one",
		"{type: http, package: github, url: 'https://e.example'}":                                         "only for type mcp",
	} {
		c, err := Parse([]byte(pkgServer + "sources: {s: " + src + "}"))
		if err == nil {
			err = c.Validate()
		}
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", src, err)
		}
	}
	c, _ := Parse([]byte(pkgServer + "sources: {s: {type: mcp, package: github, read: {tool: t}}}"))
	if got := c.Sources["s"].Command; len(got) != 2 || got[0] != "github-mcp-server" {
		t.Errorf("command not resolved: %v", got)
	}
	c, _ = Parse([]byte("server: {mcp_packages: {bad: {env: [PATH], hosts: ['a b']}}}"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "command is required") || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("package validation: %v", err)
	}
}

func TestPackageEgressHosts(t *testing.T) {
	c, _ := Parse([]byte(pkgServer + "sources: {s: {type: mcp, package: github, read: {tool: t}}, o: {type: mcp, command: [x], read: {tool: t}}}\n" +
		"agents: {with: {kind: claude, mcp: [s]}, without: {kind: claude, mcp: [o]}}"))
	has := func(a string) (api, ghe bool) {
		hosts, _ := c.AgentEgress(c.Agents[a])
		for _, h := range hosts {
			api = api || (h.Host == "api.github.com" && h.Port == 443)
			ghe = ghe || (h.Host == "ghe.example" && h.Port == 8443)
		}
		return
	}
	if api, ghe := has("with"); !api || !ghe {
		t.Error("package hosts missing")
	}
	if api, ghe := has("without"); api || ghe {
		t.Error("package hosts leaked to another agent")
	}
}

func TestBridgeEgressHosts(t *testing.T) {
	t.Setenv("AGW_E", "v")
	c, _ := Parse([]byte(pkgServer + "sources: {s: {type: mcp, package: github, read: {tool: t}, env: {GITHUB_PERSONAL_ACCESS_TOKEN: 'env:AGW_E'}}}\n" +
		"agents: {a: {kind: claude, mcp: [s]}}"))
	if hosts, _ := c.AgentEgress(c.Agents["a"]); slices.ContainsFunc(hosts, func(h HostPort) bool { return h.Host == "api.github.com" }) {
		t.Error("the agent must not reach the package's hosts: its bridge does")
	}
	hosts := c.BridgeEgress(c.Sources["s"])
	if len(hosts) != 2 || hosts[0].Host != "api.github.com" || hosts[0].Port != 443 {
		t.Errorf("bridge hosts %v", hosts)
	}
}

// A rule that reads a provider's header needs that provider's webhook.
func TestProviderMismatchWarnings(t *testing.T) {
	cfg, err := Parse([]byte(`
server: { sandbox: none }
sources:
  github-hooks: { type: webhook, signature: github, secret: env:X }
  gitlab-hooks: { type: webhook, signature: token, token_header: X-Gitlab-Token, secret: env:X }
  hello-hook: { type: webhook, signature: token, token_header: X-Key, secret: env:X }
  plain: { type: webhook, signature: sha256, secret: env:X }
rules:
  - { name: gh-ok, source: github-hooks, when: 'headers["x-github-event"] == "pull_request"', action: { cmd: [x] } }
  - { name: gh-bad, source: hello-hook, when: 'headers["x-github-event"] == "pull_request"', action: { cmd: [x] } }
  - { name: gl-ok, source: gitlab-hooks, when: 'headers["x-gitlab-event"] == "Merge Request Hook"', action: { cmd: [x] } }
  - { name: gl-bad, source: github-hooks, when: 'headers["x-gitlab-event"] != ""', action: { cmd: [x] } }
  - { name: aws-ok, source: hello-hook, when: 'event["detail-type"] == "CloudWatch Alarm State Change"', action: { cmd: [x] } }
  - { name: aws-bad, source: plain, when: 'event["detail-type"] != ""', action: { cmd: [x] } }
  - { name: fine, source: plain, when: 'true', action: { cmd: [x] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range cfg.Warnings() {
		if strings.HasPrefix(w, "rules/") {
			got = append(got, w)
		}
	}
	want := map[string]string{
		"gh-bad":  "reads GitHub's X-GitHub-Event header but source hello-hook is not a GitHub webhook (signature: github); run `siphon connect github --webhook` and use its github-hooks source",
		"gl-bad":  "reads GitLab's X-Gitlab-Event header but source github-hooks is not a GitLab webhook",
		"aws-bad": "reads EventBridge's detail-type but source plain is not a token webhook",
	}
	if len(got) != len(want) {
		t.Fatalf("%d warnings: %v", len(got), got)
	}
	for rule, text := range want {
		found := false
		for _, g := range got {
			found = found || g == "rules/"+rule+": "+text || strings.HasPrefix(g, "rules/"+rule+": ") && strings.Contains(g, text)
		}
		if !found {
			t.Errorf("no warning for %s: %v", rule, got)
		}
	}
}

func TestWebhookSignatureOptionsValidate(t *testing.T) {
	for name, tc := range map[string]struct{ extra, want string }{
		"slack ok":            {"signature: slack", ""},
		"stripe ok":           {"signature: stripe", ""},
		"prefix ok":           {"signature: sha256\n    signature_header: X-S\n    signature_prefix: v1=", ""},
		"prefix needs sha256": {"signature: slack\n    signature_prefix: v1=", "signature_prefix is only for"},
		"slack no header":     {"signature: slack\n    signature_header: X-S", "not allowed with signature slack"},
		"separator ok":        {"signature: sha256\n    signature_header: X-S\n    timestamp_header: X-T\n    timestamp_separator: ':'", ""},
		"separator needs ts":  {"signature: sha256\n    signature_header: X-S\n    timestamp_separator: ':'", "timestamp_separator needs"},
	} {
		y := "server: { sandbox: none }\nsources:\n  w:\n    type: webhook\n    secret: env:K\n    " + tc.extra + "\n"
		t.Setenv("K", "k")
		cfg, err := Parse([]byte(y))
		if err == nil {
			err = cfg.Validate()
		}
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A package that talks to the user's own server (url_env): the bridge may
// reach that host:port, and a private one only when it is listed.
func TestBridgeEgressURLEnv(t *testing.T) {
	const pkgs = "mcp_packages: {gitea: {command: [gitea-mcp], env: [GITEA_ACCESS_TOKEN, GITEA_HOST], url_env: [GITEA_HOST]}}"
	build := func(host, extra string) *Config {
		t.Setenv("T_TOK", "tok")
		t.Setenv("T_HOST", host)
		c, err := Parse([]byte("server: {" + pkgs + extra + "}\nsources: {g: {type: mcp, package: gitea, env: {GITEA_ACCESS_TOKEN: 'env:T_TOK', GITEA_HOST: 'env:T_HOST'}}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// a public host, default port from the scheme
	c := build("https://git.example.org", "")
	if err := c.BridgeCheck(c.Sources["g"]); err != nil {
		t.Fatal(err)
	}
	if got := c.BridgeEgress(c.Sources["g"]); len(got) != 1 || got[0].Host != "git.example.org" || got[0].Port != 443 || got[0].AllowPrivate {
		t.Errorf("public: %v", got)
	}
	if got := build("http://git.example.org:3000", "").BridgeEgress(build("http://git.example.org:3000", "").Sources["g"]); len(got) != 1 || got[0].Port != 3000 {
		t.Errorf("explicit port: %v", got)
	}
	// a private host is refused, and not allowed through, unless listed
	c = build("http://10.0.0.5:3000", "")
	if err := c.BridgeCheck(c.Sources["g"]); err == nil || !strings.Contains(err.Error(), "private_endpoints") {
		t.Errorf("private unlisted: %v", err)
	}
	if got := c.BridgeEgress(c.Sources["g"]); len(got) != 0 {
		t.Errorf("private unlisted leaked into egress: %v", got)
	}
	c = build("http://10.0.0.5:3000", ", services: {private_endpoints: ['10.0.0.5:3000']}")
	if err := c.BridgeCheck(c.Sources["g"]); err != nil {
		t.Fatalf("listed: %v", err)
	}
	if got := c.BridgeEgress(c.Sources["g"]); len(got) != 1 || !got[0].AllowPrivate || !got[0].NoLinkLocal || got[0].Host != "10.0.0.5" {
		t.Errorf("listed: %v", got)
	}
	// url_env must be one of env
	if _, err := Parse([]byte("server: {mcp_packages: {x: {command: [x], env: [A], url_env: [B]}}}\n")); err == nil {
		c, _ := Parse([]byte("server: {mcp_packages: {x: {command: [x], env: [A], url_env: [B]}}}\n"))
		if c.Validate() == nil {
			t.Error("url_env outside env accepted")
		}
	}
}

func TestScheduleSource(t *testing.T) {
	base := "agents:\n  a: { prompt: hi }\nsources:\n  s: "
	cases := []struct{ name, src, want string }{
		{"cron", `{type: schedule, at: "0 6 * * *"}`, ""},
		{"descriptor", `{type: schedule, at: "@daily", timezone: Europe/London, catch_up: none, data: {x: 1}}`, ""},
		{"every", `{type: schedule, at: "every 5m"}`, ""},
		{"every too short", `{type: schedule, at: "every 30s"}`, "at least"},
		{"@every 1s", `{type: schedule, at: "@every 1s"}`, "at least"},
		{"@every 1ms", `{type: schedule, at: "@every 1ms"}`, "at least"},
		{"@every padded", `{type: schedule, at: "  @every 1s"}`, "at least"},
		{"TZ in at", `{type: schedule, at: "TZ=Europe/London 0 6 * * *"}`, "use timezone:"},
		{"CRON_TZ in at", `{type: schedule, at: "CRON_TZ=Asia/Tokyo 0 6 * * *"}`, "use timezone:"},
		{"data not JSON", `{type: schedule, at: "@daily", data: {x: .inf}}`, "cannot be encoded"},
		{"data too big", `{type: schedule, at: "@daily", data: {x: "` + strings.Repeat("a", 17<<10) + `"}}`, "16 KiB"},
		{"every bad", `{type: schedule, at: "every soon"}`, "bad interval"},
		{"at missing", `{type: schedule}`, "at is required"},
		{"bad cron", `{type: schedule, at: "61 * * * *"}`, "bad at"},
		{"bad zone", `{type: schedule, at: "@daily", timezone: Mars/Base}`, "bad timezone"},
		{"bad catch_up", `{type: schedule, at: "@daily", catch_up: all}`, "catch_up must be"},
		{"reserved data", `{type: schedule, at: "@daily", data: {fired_at: x}}`, "reserved"},
		{"url", `{type: schedule, at: "@daily", url: "http://x"}`, "no url, read, poll"},
		{"poll", `{type: schedule, at: "@daily", poll: 1m}`, "no url, read, poll"},
		{"secret", `{type: schedule, at: "@daily", secret: "env:X"}`, "no url, read, poll"},
		{"at on http", `{type: http, url: "http://x", at: "@daily"}`, "only for type schedule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(base + tc.src))
			if err != nil {
				t.Fatal(err)
			}
			err = c.Validate()
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if s := c.Sources["s"]; s.Type == "schedule" && (s.Polled() || (tc.want == "" && s.Poll != 0)) {
				t.Fatal("schedule must not be polled")
			}
		})
	}
}

func TestScheduleAgentWarning(t *testing.T) {
	for at, want := range map[string]bool{"every 5m": true, "*/10 * * * *": true, "0 * * * *": false, "@daily": false} {
		c, err := Parse([]byte("agents:\n  a: { prompt: hi }\nsources:\n  s: { type: schedule, at: \"" + at + "\" }\nrules:\n  - { name: r, source: s, when: \"true\", cooldown: 1m, action: { agent: a } }\n"))
		if err != nil {
			t.Fatal(err)
		}
		got := slices.ContainsFunc(c.Warnings(), func(w string) bool { return strings.Contains(w, "agent_runs_per_day") })
		if got != want {
			t.Errorf("%s: warning=%v want %v", at, got, want)
		}
	}
}

func TestScheduleRuleDefaultsToEach(t *testing.T) {
	y := "sources:\n  s: { type: schedule, at: \"@daily\" }\nrules:\n  - { name: a, source: s, when: \"true\", action: { cmd: [x] } }\n  - { name: b, source: s, when: \"true\", on: edge, action: { cmd: [x] } }\n"
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if r := c.Rules[0]; r.On != "each" || r.ID != "event.scheduled_at" {
		t.Errorf("default: %+v", r)
	}
	w := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(w, "rule b: on: edge on schedule source s") || strings.Contains(w, "rule a:") {
		t.Errorf("warnings: %s", w)
	}
}

func TestScheduleKeyedByInstant(t *testing.T) {
	for at, want := range map[string]bool{"@every 5m": true, "every 2h": true, "*/5 * * * *": true, "30 1 * * *": false, "0 * * * *": false, "@daily": false} {
		sc, err := ParseSchedule(at)
		if err != nil {
			t.Fatal(at, err)
		}
		if got := ScheduleKeyedByInstant(sc); got != want {
			t.Errorf("%s: %v want %v", at, got, want)
		}
	}
}

func TestRetentionValidation(t *testing.T) {
	setenv(t)
	for _, tc := range []struct{ yaml, want string }{
		{"", ""},
		{"server: { retention: { jobs: 24h, audit: 48h, seen_events: 25h } }", ""},
		{"server: { retention: { audit: 23h } }", "server.retention.audit: must be at least 24h"},
	} {
		c, err := Parse([]byte(tc.yaml))
		if err != nil {
			t.Fatal(err)
		}
		err = c.Validate()
		if tc.want == "" && (err != nil || c.Server.Retention.Jobs != 0 && tc.yaml == "") {
			t.Fatalf("unexpected: %v", err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("want %q, got %v", tc.want, err)
		}
	}
}

func TestMetricsTokenValidation(t *testing.T) {
	setenv(t)
	const scrape = "scrape-secret-0123456789abcdef012345"
	t.Setenv("AGW_SCRAPE", scrape)
	t.Setenv("AGW_SHORT", "short")
	t.Setenv("AGW_SAME", "tok-secret-0123456789abcdef0123456789")
	for _, tc := range []struct{ yaml, want string }{
		{"", ""},
		{"server: {token: env:AGW_TOKEN, metrics: {token: env:AGW_SCRAPE}}", ""},
		{"server: {metrics: {token: " + scrape + "}}", "inline secret"},
		{"server: {metrics: {token: env:AGW_SHORT}}", "at least 32"},
		{"server: {token: env:AGW_TOKEN, metrics: {token: env:AGW_SAME}}", "must differ"},
	} {
		c, err := Parse([]byte(tc.yaml))
		if err != nil {
			t.Fatal(err)
		}
		err = c.Validate()
		if tc.want != "" {
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%q: want %q, got %v", tc.yaml, tc.want, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected: %v", tc.yaml, err)
		}
		if c.Server.Metrics.Token.isSet() && !slices.Contains(c.Secrets(), scrape) {
			t.Fatal("scrape token missing from Secrets()")
		}
	}
}

func TestOAuthSource(t *testing.T) {
	t.Setenv("AGW_CS", "s3cret-value")
	t.Setenv("AGW_B", "b")
	src := func(pub, auth, extra string) string {
		return "server: {public_url: \"" + pub + "\"}\nsources: {s: {type: mcp, url: \"https://m/mcp\", " + extra + "auth: {" + auth + "}}}"
	}
	for name, tc := range map[string]struct{ y, want string }{
		"ok":          {src("https://gw.example", "oauth: {}", ""), ""},
		"ok loopback": {src("http://127.0.0.1:8080", "oauth: {client_id: id, client_secret: env:AGW_CS, scopes: [a]}", ""), ""},
		"localhost":   {src("http://localhost", "oauth: {}", ""), ""},
		"no public":   {src("", "oauth: {}", ""), "needs server.public_url"},
		"http public": {src("http://gw.example", "oauth: {}", ""), "must be https"},
		"bearer":      {src("https://gw.example", "bearer: env:AGW_B, oauth: {}", ""), "not allowed with auth.bearer"},
		"header":      {src("https://gw.example", "oauth: {}", "headers: {authorization: x}, "), "Authorization header"},
		"secret only": {src("https://gw.example", "oauth: {client_secret: env:AGW_CS}", ""), "needs client_id"},
		"command":     {"server: {public_url: \"https://gw.example\"}\nsources: {s: {type: mcp, command: [/bin/x], auth: {oauth: {}}}}", "only for a remote MCP source"},
	} {
		c, err := Parse([]byte(tc.y))
		if err == nil {
			err = c.Validate()
		}
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err=%v want %q", name, err, tc.want)
		}
	}
	c, _ := Parse([]byte(src("https://gw.example", "oauth: {client_id: id, client_secret: env:AGW_CS}", "")))
	if err := c.Validate(); err != nil || !slices.Contains(c.Secrets(), "s3cret-value") {
		t.Errorf("client_secret not in Secrets: %v", err)
	}
}

func TestOIDCConfig(t *testing.T) {
	t.Setenv("OIDC_SECRET", "s3cret")
	const ok = `
server: {public_url: "https://s.example.com", oidc: {issuer: "https://idp.example.com", client_id: siphon, client_secret: "env:OIDC_SECRET", roles: {viewer: {emails: [a@example.com]}}}}
`
	cases := []struct{ name, yaml, want string }{
		{"ok", ok, ""},
		{"loopback http", strings.Replace(ok, "https://idp.example.com", "http://127.0.0.1:9", 1), ""},
		{"http issuer", strings.Replace(ok, "https://idp.example.com", "http://idp.example.com", 1), "server.oidc.issuer"},
		{"no client_id", strings.Replace(ok, "client_id: siphon, ", "", 1), "client_id"},
		{"no public_url", strings.Replace(ok, `public_url: "https://s.example.com", `, "", 1), "public_url"},
		{"no roles", strings.Replace(ok, "roles: {viewer: {emails: [a@example.com]}}", "roles: {viewer: {}}", 1), "server.oidc.roles"},
		{"scopes without openid", strings.Replace(ok, "roles:", "scopes: [email], roles:", 1), "openid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.yaml))
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
	c, _ := Parse([]byte(ok))
	if !slices.Contains(c.Secrets(), "s3cret") {
		t.Error("client_secret missing from Secrets()")
	}
	if !c.Server.OIDC.TokenLoginOn() || c.Server.OIDC.GroupsClaimOrDefault() != "groups" || len(c.Server.OIDC.ScopesOrDefault()) != 3 {
		t.Error("defaults wrong")
	}
	if _, err := Parse([]byte(strings.Replace(ok, "viewer:", "root:", 1))); err == nil {
		t.Error("unknown role name should fail decoding")
	}
}
