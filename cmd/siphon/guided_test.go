package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/store"
)

func TestConnectGitHubGitLab(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/user" && r.Header.Get("PRIVATE-TOKEN") == "glpat-SECRET2" {
			w.Write([]byte(`{"username":"olaf"}`))
			return
		}
		http.Error(w, "nope", 401)
	}))
	defer gl.Close()
	u, _ := url.Parse(gl.URL)
	e := newCLIEnvCfg(t, `, services: { private_endpoints: ["`+u.Host+`"] }`, "")

	// a plain secret is refused, and nothing is saved
	code, _, er := e.do("connect", "github", "--name", "gh2", "--token", "ghp_PLAIN", "--no-test")
	if code != 2 || !strings.Contains(er, "hint:") || strings.Contains(er, "ghp_PLAIN") {
		t.Fatalf("plain: %d %s", code, er)
	}
	// no terminal: the missing pieces are listed
	if code, _, er = e.do("connect", "github", "--no-test"); code != 2 || !strings.Contains(er, "missing: --token") {
		t.Fatalf("missing: %d %s", code, er)
	}
	if code, _, er = e.do("connect", "gitlab", "--token", "-"); code != 2 || !strings.Contains(er, "--project") {
		t.Fatalf("gitlab missing: %d %s", code, er)
	}
	e.stdin = "ghp_SECRET1\n"
	out := e.ok("connect", "github", "--name", "ghub", "--token", "-", "--webhook", "--no-test")
	m := regexp.MustCompile(`secret: ([0-9a-f]{64})`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "shown once") || !strings.Contains(out, `connected GitHub: "ghub"`) {
		t.Fatalf("github output: %s", out)
	}
	sf := filepath.Join(e.dir, "secrets", "sources-ghub-token")
	_ = sf
	for _, args := range [][]string{{"get", "sources", "ghub"}, {"get", "sources", "ghub-hooks"}, {"history", "1"}, {"get", "audit"}} {
		if o := e.ok(args...); strings.Contains(o, m[1]) || strings.Contains(o, "ghp_SECRET1") {
			t.Fatalf("%v shows a secret", args)
		}
	}
	// GitLab with its test, and from a token file
	e.write("gl.tok", "glpat-SECRET2\n")
	out = e.ok("connect", "gitlab", "--name", "gl", "--base", gl.URL, "--project", "grp/proj", "--token", "@"+filepath.Join(e.dir, "gl.tok"))
	if !strings.Contains(out, "test: ok (olaf") {
		t.Fatalf("gitlab: %s", out)
	}
	// a failing test reports it and exits 1
	e.stdin = "glpat-WRONG\n"
	code, out, er = e.do("connect", "gitlab", "--name", "gl2", "--base", gl.URL, "--project", "grp/proj", "--token", "-")
	if code != 1 || !strings.Contains(er, "test failed") || !strings.Contains(er, "hint:") {
		t.Fatalf("failing test: %d %s %s", code, out, er)
	}
	// -o json carries the one-time secret as a field
	e.stdin = "ghp_SECRET3\n"
	out = e.ok("connect", "github", "--name", "g3", "--token", "-", "--webhook", "--no-test", "-o", "json")
	var j struct {
		Connected map[string]any `json:"connected"`
	}
	if json.Unmarshal([]byte(out), &j) != nil || len(str(j.Connected, "hook_secret")) != 64 {
		t.Fatalf("json: %s", out)
	}
	if code, _, _ := e.do("connect", "nothing"); code != 2 {
		t.Fatal("unknown target")
	}
	if code, _, _ := e.do("connect"); code != 2 {
		t.Fatal("no target")
	}
}

const awsServer = `, aws: {profiles: [p1]}, mcp_packages: {aws-cloudwatch: {command: [cw], hosts: ['logs.{region}.amazonaws.com'], env: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION, AWS_DEFAULT_REGION, AWS_EC2_METADATA_DISABLED, AWS_CONFIG_FILE, AWS_SHARED_CREDENTIALS_FILE]}}`

func TestConnectAWS(t *testing.T) {
	e := newCLIEnvCfg(t, awsServer, "")
	if code, _, er := e.do("connect", "aws", "--name", "prod", "--no-test"); code != 2 || !strings.Contains(er, "--region") || !strings.Contains(er, "--profile") {
		t.Fatalf("missing: %d %s", code, er)
	}
	if code, _, er := e.do("connect", "aws", "--name", "prod", "--region", "eu-west-1", "--role-arn", "arn:aws:iam::123456789012:role/x", "--secret-access-key", "plain", "--no-test"); code != 2 || strings.Contains(er, "plain\n") {
		t.Fatalf("plain key: %d %s", code, er)
	}
	out := e.ok("connect", "aws", "--name", "prod", "--region", "eu-west-1", "--profile", "p1", "--webhook", "--no-test")
	if !strings.Contains(out, `connected AWS: "prod"`) || !strings.Contains(out, "X-Siphon-Key") || !strings.Contains(out, "shown once") {
		t.Fatalf("aws: %s", out)
	}
	if o := e.ok("get", "credentials"); !strings.Contains(o, "prod") {
		t.Fatalf("credential: %s", o)
	}
}

func TestConnectModelAndLogin(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen3:8b"},{"name":"tiny:1b"}]}`))
		case r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer sk-secret-123":
			w.Write([]byte(`{"data":[{"id":"gpt-oss-20b"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer stub.Close()
	u, _ := url.Parse(stub.URL)
	e := newCLIEnvCfg(t, `, models: { private_endpoints: ["`+u.Host+`"] }`, "")
	out := e.ok("connect", "model", "ollama", "--name", "local", "--url", stub.URL)
	if !strings.Contains(out, "test: ok, 2 models: qwen3:8b, tiny:1b") {
		t.Fatalf("ollama: %s", out)
	}
	if code, _, er := e.do("connect", "model", "openai", "--name", "oai", "--url", stub.URL+"/v1", "--api-key", "sk-secret-123"); code != 2 || strings.Contains(er, "sk-secret-123") {
		t.Fatalf("plain api key: %d %s", code, er)
	}
	e.stdin = "sk-secret-123\n"
	if out = e.ok("connect", "model", "openai", "--name", "oai", "--url", stub.URL+"/v1", "--api-key", "-"); !strings.Contains(out, "gpt-oss-20b") {
		t.Fatalf("openai: %s", out)
	}
	if code, _, _ := e.do("connect", "model"); code != 2 {
		t.Fatal("model without a preset")
	}
	if code, _, _ := e.do("connect", "model", "bogus", "--no-test"); code != 4 && code != 3 && code != 1 {
		t.Fatalf("bad preset: %d", code)
	}
	if out = e.ok("test", "model", "local"); !strings.Contains(out, "ok: 2 models") {
		t.Fatalf("test model: %s", out)
	}
	if code, _, _ := e.do("test", "model", "nope"); code != 4 {
		t.Fatalf("unknown model: %d", code)
	}
	if code, _, _ := e.do("test", "service", "nope"); code != 4 {
		t.Fatalf("unknown service: %d", code)
	}
	// logins: exactly one kind, secrets only from - / @file
	e.stdin = "sk-LEAKY-1\n"
	if out = e.ok("connect", "login", "claude", "--name", "k1", "--api-key", "-"); !strings.Contains(out, "status apikey") || strings.Contains(out, "LEAKY") {
		t.Fatalf("login: %s", out)
	}
	for _, args := range [][]string{{"connect", "login", "claude"}, {"connect", "login", "claude", "--api-key", "-", "--file", "x"}, {"connect", "login", "claude", "--api-key", "sk-plain"}, {"connect", "login", "nobody", "--api-key", "-"}, {"connect", "login", "claude", "--setup-token", "tok-plain"}} {
		if code, _, er := e.do(args...); code != 2 || strings.Contains(er, "sk-plain") || strings.Contains(er, "tok-plain") {
			t.Fatalf("%v: %d %s", args, code, er)
		}
	}
	e.write("login.json", "not json")
	if code, _, _ := e.do("connect", "login", "claude", "--name", "bad", "--file", filepath.Join(e.dir, "login.json")); code != 3 {
		t.Fatalf("bad login file: %d", code)
	}
	if o := e.ok("get", "connections"); !strings.Contains(o, "k1") || !strings.Contains(o, "oai") || strings.Contains(o, "LEAKY") {
		t.Fatalf("connections: %s", o)
	}
}

func TestTestLastWhy(t *testing.T) {
	e := newCLIEnv(t)
	e.ok("apply", "-f", e.write("v.yaml", "rules:\n  - { name: big, source: gh, when: \"event.v > 5\", action: { cmd: [echo, \"{{.event.v}}\"] } }\n"), "--yes")
	// why: no events yet
	out := e.ok("why", "big")
	if !strings.Contains(out, "✗ the source has not produced an event yet") || !strings.Contains(out, "Likely reason: Source gh has not produced") || !strings.Contains(out, "Next: send an event") {
		t.Fatalf("no events:\n%s", out)
	}
	if code, _, _ := e.do("test", "big", "--last"); code != 4 {
		t.Fatalf("--last without an event: %d", code)
	}
	// condition false
	store.SetSourceEvent(e.st.DB, "gh", map[string]any{"v": 1})
	out = e.ok("why", "big")
	if !strings.Contains(out, "✗ the last event does not satisfy the condition") || !strings.Contains(out, "Likely reason: The last event does not satisfy") || !strings.Contains(out, "Next: siphon test big --last") || !strings.Contains(out, "✓ enabled") {
		t.Fatalf("condition false:\n%s", out)
	}
	if out = e.ok("test", "big", "--last"); !strings.Contains(out, "no fire") {
		t.Fatalf("test --last no fire: %s", out)
	}
	// would fire; test with --last and with a file and stdin
	store.SetSourceEvent(e.st.DB, "gh", map[string]any{"v": 9, "token": "tok-LEAK"})
	out = e.ok("test", "big", "--last")
	if !strings.Contains(out, "echo 9") || strings.Contains(out, "LEAK") {
		t.Fatalf("test --last: %s", out)
	}
	if out = e.ok("why", "big"); !strings.Contains(out, "Nothing blocks it") {
		t.Fatalf("would fire:\n%s", out)
	}
	if out = e.ok("test", "big", e.write("ev.json", `{"v": 7}`)); !strings.Contains(out, "echo 7") {
		t.Fatalf("test file: %s", out)
	}
	e.stdin = `{"v": 3}`
	if out = e.ok("test", "big", "-"); !strings.Contains(out, "no fire") {
		t.Fatalf("test stdin: %s", out)
	}
	var tv map[string]any
	if json.Unmarshal([]byte(e.ok("test", "big", "--last", "-o", "json")), &tv) != nil || tv["fires"] == nil {
		t.Fatal("test json")
	}
	if code, _, _ := e.do("test", "big", e.write("bad.json", "{")); code != 2 {
		t.Fatalf("bad event: %d", code)
	}
	if code, _, _ := e.do("test", "big"); code != 2 {
		t.Fatalf("no event: %d", code)
	}
	if code, _, _ := e.do("test", "nope", "--last"); code != 4 {
		t.Fatalf("unknown rule: %d", code)
	}
	// disabled
	e.ok("disable", "big")
	if out = e.ok("why", "big"); !strings.Contains(out, "✗ disabled") || !strings.Contains(out, "Next: siphon enable big") {
		t.Fatalf("disabled:\n%s", out)
	}
	var x map[string]any
	if json.Unmarshal([]byte(e.ok("why", "big", "-o", "json")), &x) != nil || x["reasons"] == nil {
		t.Fatal("why json")
	}
	if code, _, _ := e.do("why", "nope"); code != 4 {
		t.Fatalf("why unknown: %d", code)
	}
}

func TestNewTaskFlags(t *testing.T) {
	e := newCLIEnv(t)
	flags := []string{"new", "task", "--name", "n1", "--webhook", "alerts", "--when", `event.sev == "high"`, "--on", "each", "--id", "event.id", "--cmd", `["echo","{{.event.msg}}"]`, "--cooldown", "1m"}
	printed := e.ok(append(flags, "--print")...)
	for _, want := range []string{"alerts:", "token_header: X-Siphon-Key", "name: n1", "on: each", "cooldown: 1m", "cmd: [echo, '{{.event.msg}}']"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("missing %q in:\n%s", want, printed)
		}
	}
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("--print stored something")
	}
	// the printed YAML is valid for apply (the webhook needs its secret)
	e.write("sec", "pipe-SECRET")
	e.stdin = printed
	e.ok("apply", "-f", "-", "--yes", "--secret", "sources/alerts.secret=@"+filepath.Join(e.dir, "sec"))
	if o := e.ok("get", "rules", "n1"); !strings.Contains(o, "source: alerts") {
		t.Fatalf("applied from --print: %s", o)
	}
	// the one-step form: generated secret, shown once
	out := e.ok("new", "task", "--name", "n2", "--webhook", "alerts2", "--when", "true", "--cmd", `["echo","hi"]`, "--yes")
	m := regexp.MustCompile(`secret: ([0-9a-f]{64})`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "shown once") || !strings.Contains(out, "/hook/alerts2") || !strings.Contains(out, "applied revision") {
		t.Fatalf("new task: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "secrets", "sources-alerts2-secret")); string(b) != m[1] {
		t.Fatal("secret file differs from the printed one")
	}
	for _, args := range [][]string{{"get", "sources", "alerts2"}, {"history", "2"}, {"get", "audit"}} {
		if strings.Contains(e.ok(args...), m[1]) {
			t.Fatalf("%v shows the secret", args)
		}
	}
	// -o json has it as a field
	out = e.ok("new", "task", "--name", "n3", "--webhook", "alerts3", "--when", "true", "--cmd", `["echo"]`, "--yes", "-o", "json")
	var j struct {
		Applied bool              `json:"applied"`
		Webhook map[string]string `json:"webhook"`
	}
	if json.Unmarshal([]byte(out), &j) != nil || !j.Applied || len(j.Webhook["secret"]) != 64 {
		t.Fatalf("json: %s", out)
	}
	// polling source on an existing source; dry run changes nothing
	e.ok("new", "task", "--name", "n4", "--poll", "disk", "--url", "https://example.com/x", "--every", "1m", "--when", "event.v > 1", "--repeat", "1h", "--cmd", `["echo"]`, "--dry-run")
	if code, _, _ := e.do("get", "rules", "n4"); code != 4 {
		t.Fatal("dry-run created the rule")
	}
	e.ok("new", "task", "--name", "n5", "--source", "gh", "--when", "true", "--cmd", `["echo","five"]`, "--yes")
	// refusals: exit 2, nothing sent
	for _, args := range [][]string{
		{"new", "task", "--name", "n5", "--source", "gh", "--when", "true", "--cmd", `["x"]`, "--yes"},                            // rule exists
		{"new", "task", "--name", "n6", "--webhook", "alerts", "--when", "true", "--cmd", `["x"]`, "--yes"},                       // source exists
		{"new", "task", "--name", "n6", "--source", "gh", "--when", "true", "--on", "each", "--cmd", `["x"]`},                     // each needs id
		{"new", "task", "--name", "n6", "--source", "gh", "--when", "true", "--agent", "a"},                                       // agent needs cooldown
		{"new", "task", "--name", "n6", "--source", "gh", "--webhook", "w", "--when", "true", "--cmd", `["x"]`},                   // two sources
		{"new", "task", "--name", "n6", "--source", "gh", "--when", "true", "--cmd", `not json`},                                  // bad cmd
		{"new", "task", "--name", "n6", "--source", "gh", "--when", "true", "--cmd", `["x"]`, "--agent", "a", "--cooldown", "1m"}, // two actions
		{"new", "task", "--source", "gh"}, // missing
		{"new", "task"},                   // no terminal, no flags
		{"new", "job"},
	} {
		if code, _, er := e.do(args...); code != 2 {
			t.Errorf("%v: exit %d %s", args, code, er)
		}
	}
	if code, _, er := e.do("new", "task", "--source", "gh"); !strings.Contains(er, "--name") || !strings.Contains(er, "--when") || code != 2 {
		t.Fatalf("missing list: %s", er)
	}
}

func TestNewTaskWizard(t *testing.T) {
	e := newCLIEnv(t)
	e.tty = true
	// name, source (Enter takes the existing gh), when (Enter: the starter), on, id, action, command, cooldown
	script := "wiz1\n\n\neach\nevent.number\ncmd\necho hello world\n\n"
	e.stdin = script
	wiz := e.ok("new", "task", "--print")
	e.tty = false
	flag := e.ok("new", "task", "--name", "wiz1", "--source", "gh", "--when", `event.action == "opened"`, "--on", "each", "--id", "event.number", "--cmd", `["echo","hello","world"]`, "--print")
	if wiz != flag {
		t.Fatalf("wizard and flags differ:\n%s\n---\n%s", wiz, flag)
	}
	// and run for real, confirming with y
	e.tty = true
	e.stdin = script + "y\n"
	out := e.ok("new", "task")
	if !strings.Contains(out, "applied revision") {
		t.Fatalf("wizard apply: %s", out)
	}
	if y := e.ok("get", "rules", "wiz1"); !strings.Contains(y, "event.number") {
		t.Fatalf("not applied: %s", y)
	}
	// a new webhook source and a new agent through the wizard
	e.ok("connect", "login", "claude", "--name", "cl", "--api-key", "@"+e.write("k", "sk-x"))
	e.tty = true
	e.stdin = "wiz2\nnew\nwebhook\nhooks2\n\nedge\n\nnew-agent\nhelper\ncl\nSummarise the event\nget_me\n\ny\n"
	out = e.ok("new", "task")
	if !strings.Contains(out, "shown once") || !strings.Contains(out, "applied revision") {
		t.Fatalf("wizard 2: %s", out)
	}
	if y := e.ok("get", "agents", "helper"); !strings.Contains(y, "credential: cl") || !strings.Contains(y, "get_me") {
		t.Fatalf("agent: %s", y)
	}
	if y := e.ok("get", "rules", "wiz2"); !strings.Contains(y, "agent: helper") || !strings.Contains(y, "cooldown: 10m") {
		t.Fatalf("rule: %s", y)
	}
}

func TestHelpListsGuidedCommands(t *testing.T) {
	var h helpDoc
	e := newCLIEnv(t)
	if json.Unmarshal([]byte(e.ok("help", "--json")), &h) != nil {
		t.Fatal("help json")
	}
	have := map[string]cmdDoc{}
	for _, c := range h.Commands {
		have[c.Name] = c
	}
	for _, n := range []string{"connect", "test", "why", "new"} {
		c, ok := have[n]
		if !ok || c.Example == "" || c.Mode != "client" || len(c.Flags) == 0 && n != "why" {
			t.Errorf("%s: %+v", n, c)
		}
	}
	// connect and new own -url, so the server URL is not a flag there
	for _, n := range []string{"connect", "new"} {
		for _, f := range have[n].Flags {
			if f.Name == "url" && strings.Contains(f.Description, "siphon URL") {
				t.Errorf("%s: -url is the server flag", n)
			}
		}
	}
}
