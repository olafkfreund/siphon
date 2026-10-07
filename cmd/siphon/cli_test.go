package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/client"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/job"
	"github.com/olafkfreund/siphon/internal/store"
	"github.com/olafkfreund/siphon/internal/web"
)

const cliTok = "tok-0123456789abcdef0123456789abcdef"

// cliEnv is a real siphon web server on a temp store, and a login for it.
type cliEnv struct {
	t     *testing.T
	dir   string
	url   string
	st    *store.Store
	tokf  string
	cfgp  string
	stdin string
	tty   bool
	edit  func(path string) error
}

func newCLIEnv(t *testing.T) *cliEnv { return newCLIEnvCfg(t, "", "") }

// newCLIEnvCfg adds settings to the server section and lines to the file's top level.
func newCLIEnvCfg(t *testing.T, serverExtra, top string) *cliEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGW_HOOK", "s3cret")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	for _, k := range []string{"SIPHON_URL", "SIPHON_TOKEN", "SIPHON_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	cfgp := filepath.Join(dir, "siphon.yaml")
	os.WriteFile(cfgp, []byte("server: { sandbox: none, db: "+dir+"/s.db"+serverExtra+" }\n"+top+"sources:\n  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }\nrules:\n  - { name: r1, source: gh, when: \"true\", action: { cmd: [echo, one] } }\n"), 0o600)
	cfg, err := config.Load(cfgp)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := job.New(cfg, st, time.Now)
	srv := httptest.NewServer(web.New(web.Options{Token: cliTok, Store: st, Config: p.Config, Apply: p.Apply, ConfigPath: cfgp, Decide: p.Decide, Hooks: p.Webhooks(), Now: time.Now}))
	t.Cleanup(srv.Close)
	tokf := filepath.Join(dir, "token")
	os.WriteFile(tokf, []byte(cliTok+"\n"), 0o600)
	return &cliEnv{t: t, dir: dir, url: srv.URL, st: st, tokf: tokf, cfgp: cfgp}
}

// do runs one command with the env's login (SIPHON_URL / SIPHON_TOKEN_FILE).
func (e *cliEnv) do(args ...string) (code int, out, errOut string) {
	e.t.Helper()
	e.t.Setenv("SIPHON_URL", e.url)
	e.t.Setenv("SIPHON_TOKEN_FILE", e.tokf)
	var o, er bytes.Buffer
	c := &cli{in: strings.NewReader(e.stdin), out: &o, errw: &er, isTTY: e.tty, g: globals{output: "text"}, runEditor: e.edit}
	code = c.run(args)
	return code, o.String(), er.String()
}

func (e *cliEnv) ok(args ...string) string {
	e.t.Helper()
	code, out, er := e.do(args...)
	if code != 0 {
		e.t.Fatalf("siphon %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, er)
	}
	return out
}

func (e *cliEnv) write(name, content string) string {
	p := filepath.Join(e.dir, name)
	os.WriteFile(p, []byte(content), 0o600)
	return p
}

const taskYAML = `# a task
sources:
  hook: { type: http, url: "https://example.com/x", poll: 1m }
rules:
  - name: t1   # first
    source: hook
    when: "true"
    action: { cmd: [echo, hi] }
  - { name: t2, source: gh, when: "true", action: { cmd: [echo, two] } }
`

func TestCLIGetStatusApplyFlow(t *testing.T) {
	e := newCLIEnv(t)
	if out := e.ok("get", "rules"); !strings.Contains(out, "r1") || !strings.Contains(out, "file") {
		t.Fatalf("get rules: %s", out)
	}
	if out := e.ok("get", "rules", "r1"); !strings.Contains(out, "source: gh") {
		t.Fatalf("get rule: %s", out)
	}
	if out := e.ok("status"); !strings.Contains(out, "sources: 1") || !strings.Contains(out, "rules: 1") {
		t.Fatalf("status: %s", out)
	}
	var st map[string]any
	if json.Unmarshal([]byte(e.ok("status", "-o", "json")), &st) != nil || st["approvals"] != float64(0) {
		t.Fatalf("status json: %v", st)
	}
	f := e.write("task.yaml", taskYAML)
	// --dry-run changes nothing, and shows the diff
	if out := e.ok("apply", "-f", f, "--dry-run"); !strings.Contains(out, "+") || !strings.Contains(out, "t1") {
		t.Fatalf("dry-run: %s", out)
	}
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("dry-run stored something")
	}
	// no terminal and no --yes: refuses to guess
	if code, _, er := e.do("apply", "-f", f); code != 2 || !strings.Contains(er, "--yes") {
		t.Fatalf("no tty: %d %s", code, er)
	}
	// a scripted "n" declines, "y" applies
	e.tty, e.stdin = true, "n\n"
	if out := e.ok("apply", "-f", f); !strings.Contains(out, "not applied") {
		t.Fatalf("declined: %s", out)
	}
	e.stdin = "y\n"
	if out := e.ok("apply", "-f", f); !strings.Contains(out, "applied revision 1 (3 items)") {
		t.Fatalf("apply: %s", out)
	}
	e.tty = false
	if out := e.ok("apply", "-f", f, "--yes"); !strings.Contains(out, "no change") {
		t.Fatalf("again: %s", out)
	}
	y := e.ok("get", "rules", "t1")
	if !strings.Contains(y, "source: hook") || strings.Contains(y, "name:") {
		t.Fatalf("item yaml drops name: %s", y)
	}
	// apply from stdin needs --yes
	e.stdin = "rules:\n  - { name: t3, source: gh, when: \"true\", action: { cmd: [echo, three] } }\n"
	if code, _, _ := e.do("apply", "-f", "-"); code != 2 {
		t.Fatalf("stdin without --yes: %d", code)
	}
	e.stdin = "rules:\n  - { name: t3, source: gh, when: \"true\", action: { cmd: [echo, three] } }\n"
	e.ok("apply", "-f", "-", "--yes", "--quiet")
	// enable / disable
	e.ok("disable", "t3")
	if out := e.ok("status"); !strings.Contains(out, "disabled: t3") {
		t.Fatalf("status: %s", out)
	}
	e.ok("enable", "t3")
	// history and restore
	h := e.ok("history")
	if !strings.Contains(h, "apply: 3 items") || !strings.Contains(h, "cli:") {
		t.Fatalf("history: %s", h)
	}
	if out := e.ok("history", "1"); !strings.Contains(out, "+") {
		t.Fatalf("history 1: %s", out)
	}
	if code, _, _ := e.do("restore", "1"); code != 2 { // not a tty
		t.Fatalf("restore without --yes: %d", code)
	}
	e.ok("restore", "1", "--yes")
	if out := e.ok("get", "rules"); strings.Contains(out, "t3") || !strings.Contains(out, "t1") {
		t.Fatalf("after restore: %s", out)
	}
	// delete a file item (tombstone), then reset it
	e.ok("delete", "rules", "r1", "--dry-run")
	if out := e.ok("get", "rules"); strings.Contains(out, "deleted") {
		t.Fatalf("dry-run deleted: %s", out)
	}
	e.ok("delete", "rules", "r1")
	if out := e.ok("get", "rules"); !strings.Contains(out, "deleted") {
		t.Fatalf("tombstone: %s", out)
	}
	e.ok("reset", "rules", "r1")
	if out := e.ok("history", "rules", "r1"); !strings.Contains(out, "rules/r1") {
		t.Fatalf("history filter: %s", out)
	}
	e.ok("delete", "rules", "t1")
	if code, _, _ := e.do("get", "rules", "t1"); code != 4 {
		t.Fatalf("deleted item: %d", code)
	}
}

func TestCLISecretsAndExitCodes(t *testing.T) {
	e := newCLIEnv(t)
	f := e.write("hook.yaml", "sources:\n  hk: { type: webhook, signature: github }\n")
	// a plain value is refused, nothing is sent
	code, _, er := e.do("apply", "-f", f, "--secret", "sources/hk.secret=plain-VALUE")
	if code != 2 || !strings.Contains(er, "hint:") || strings.Contains(er, "plain-VALUE") {
		t.Fatalf("plain secret: %d %s", code, er)
	}
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("stored despite refusal")
	}
	// the flag is checked before the file is read
	if code, _, er := e.do("apply", "-f", filepath.Join(e.dir, "missing.yaml"), "--secret", "sources/hk.secret=plain"); code != 2 || !strings.Contains(er, "refusing a secret") {
		t.Fatalf("flag before file: %d %s", code, er)
	}
	if code, _, _ := e.do("apply", "-f", f, "--secret", "nonsense=@x"); code != 2 {
		t.Fatalf("bad key: %d", code)
	}
	kf := e.write("hk.key", "file-SECRET\n")
	e.ok("apply", "-f", f, "--yes", "--secret", "sources/hk.secret=@"+kf)
	sf := filepath.Join(e.dir, "secrets", "sources-hk-secret")
	if b, err := os.ReadFile(sf); err != nil || string(b) != "file-SECRET" {
		t.Fatalf("secret file: %q %v", b, err)
	}
	if fi, _ := os.Stat(sf); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// from stdin
	g := e.write("hk2.yaml", "sources:\n  hk2: { type: webhook, signature: github }\n")
	e.stdin = "stdin-SECRET\n"
	e.ok("apply", "-f", g, "--yes", "--secret", "sources/hk2.secret=-")
	if b, _ := os.ReadFile(filepath.Join(e.dir, "secrets", "sources-hk2-secret")); string(b) != "stdin-SECRET" {
		t.Fatalf("stdin secret %q", b)
	}
	for _, args := range [][]string{{"get", "sources", "hk"}, {"get", "sources"}, {"history"}, {"history", "1"}} {
		if out := e.ok(args...); strings.Contains(out, "file-SECRET") || strings.Contains(out, "stdin-SECRET") {
			t.Fatalf("%v printed a secret", args)
		}
	}

	// exit codes and the error JSON
	bad := e.write("bad.yaml", "rules:\n  - { name: b, source: nope, when: \"true\", action: { cmd: [x] } }\n")
	code, _, er = e.do("apply", "-f", bad, "--dry-run")
	if code != 3 || !strings.Contains(er, "unknown source") || !strings.Contains(er, "hint:") {
		t.Fatalf("invalid: %d %s", code, er)
	}
	code, _, er = e.do("apply", "-f", bad, "--dry-run", "-o", "json")
	var je struct {
		Error  string   `json:"error"`
		Errors []string `json:"errors"`
		Hint   string   `json:"hint"`
	}
	if code != 3 || json.Unmarshal([]byte(er), &je) != nil || je.Error == "" || len(je.Errors) == 0 || !strings.Contains(je.Hint, "fix the errors") {
		t.Fatalf("invalid json: %d %s", code, er)
	}
	for args, want := range map[string]int{"get rules nope": 4, "get widgets": 2, "bogus": 2, "get": 2, "edit rules": 2, "approve x": 2, "approve 99": 4, "get -o yaml rules": 2, "get rules -zzz": 2} {
		if code, _, _ := e.do(strings.Fields(args)...); code != want {
			t.Errorf("%q: exit %d, want %d", args, code, want)
		}
	}
	if code, _, er := e.do("apply", "-f", e.write("srv.yaml", "server: { workers: 9 }\n")); code != 2 || !strings.Contains(er, "siphon.yaml") {
		t.Fatalf("server section: %d %s", code, er)
	}
	// not logged in, and a wrong token
	t.Setenv("SIPHON_URL", "")
	var o, eb bytes.Buffer
	c := &cli{in: strings.NewReader(""), out: &o, errw: &eb, g: globals{output: "text"}}
	t.Setenv("SIPHON_URL", "")
	t.Setenv("SIPHON_TOKEN_FILE", "")
	if code := c.run([]string{"get", "rules"}); code != 1 || !strings.Contains(eb.String(), "siphon login") {
		t.Fatalf("logged out: %d %s", code, eb.String())
	}
	os.WriteFile(e.tokf, []byte("wrong-token\n"), 0o600)
	if code, _, er := e.do("get", "rules"); code != 1 || !strings.Contains(er, "hint:") || strings.Contains(er, "wrong-token") {
		t.Fatalf("wrong token: %d %s", code, er)
	}
}

func TestCLILoginLogout(t *testing.T) {
	e := newCLIEnv(t)
	t.Setenv("SIPHON_URL", "")
	t.Setenv("SIPHON_TOKEN_FILE", "")
	run := func(stdin string, args ...string) (int, string, string) {
		var o, er bytes.Buffer
		c := &cli{in: strings.NewReader(stdin), out: &o, errw: &er, g: globals{output: "text"}}
		return c.run(args), o.String(), er.String()
	}
	if code, _, er := run("nope\n", "login", e.url); code != 1 || strings.Contains(er, "nope") {
		t.Fatalf("bad token: %d %s", code, er)
	}
	if _, err := os.Stat(client.Path()); err == nil {
		t.Fatal("saved a refused login")
	}
	if code, out, er := run(cliTok+"\n", "login", e.url); code != 0 || strings.Contains(out+er, cliTok) {
		t.Fatalf("login: %d %s %s", code, out, er)
	}
	if fi, err := os.Stat(client.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("client.yaml: %v", err)
	}
	if code, out, _ := run("", "get", "rules"); code != 0 || !strings.Contains(out, "r1") {
		t.Fatalf("after login: %d %s", code, out)
	}
	if code, _, _ := run("", "logout"); code != 0 {
		t.Fatal("logout")
	}
	if code, _, _ := run("", "get", "rules"); code != 1 {
		t.Fatal("still logged in")
	}
	if code, _, _ := run("", "login", "http://u:p@x"); code != 2 {
		t.Fatal("URL with credentials accepted")
	}
}

func TestCLIEditConflict(t *testing.T) {
	e := newCLIEnv(t)
	e.ok("apply", "-f", e.write("a.yaml", "rules:\n  - { name: ed, source: gh, when: \"true\", action: { cmd: [echo, one] } }\n"), "--yes")
	// a plain edit
	e.edit = func(p string) error {
		b, _ := os.ReadFile(p)
		return os.WriteFile(p, []byte(strings.Replace(string(b), "one", "edited", 1)), 0o600)
	}
	if out := e.ok("edit", "rules", "ed"); !strings.Contains(out, "saved rules/ed as revision 2") {
		t.Fatalf("edit: %s", out)
	}
	if y := e.ok("get", "rules", "ed"); !strings.Contains(y, "edited") {
		t.Fatalf("not saved: %s", y)
	}
	// unchanged text: nothing saved
	e.edit = func(string) error { return nil }
	if out := e.ok("edit", "rules", "ed"); !strings.Contains(out, "no change") {
		t.Fatalf("no change: %s", out)
	}
	// someone else saves while the editor is open: 409 (no terminal: no reopening)
	e.edit = func(p string) error {
		other := &cli{in: strings.NewReader(""), out: &bytes.Buffer{}, errw: &bytes.Buffer{}, g: globals{output: "text"}}
		_ = other
		req, _ := http.NewRequest("PUT", e.url+"/api/config/rules/ed", strings.NewReader(`{"yaml":"source: gh\nwhen: \"true\"\naction: {cmd: [echo, theirs]}\n"}`))
		req.Header.Set("Authorization", "Bearer "+cliTok)
		http.DefaultClient.Do(req)
		return os.WriteFile(p, []byte("source: gh\nwhen: \"true\"\naction: {cmd: [echo, mine]}\n"), 0o600)
	}
	code, _, er := e.do("edit", "rules", "ed")
	if code != 5 || !strings.Contains(er, "kept in") {
		t.Fatalf("conflict: %d %s", code, er)
	}
	// on a terminal the editor reopens on the current version, then saves
	n := 0
	e.tty = true
	e.edit = func(p string) error {
		n++
		if n == 1 { // lose the race once
			req, _ := http.NewRequest("PUT", e.url+"/api/config/rules/ed", strings.NewReader(`{"yaml":"source: gh\nwhen: \"true\"\naction: {cmd: [echo, theirs2]}\n"}`))
			req.Header.Set("Authorization", "Bearer "+cliTok)
			http.DefaultClient.Do(req)
		}
		return os.WriteFile(p, []byte("source: gh\nwhen: \"true\"\naction: {cmd: [echo, again]}\n"), 0o600)
	}
	out := e.ok("edit", "rules", "ed")
	if n != 2 || !strings.Contains(out, "saved rules/ed") {
		t.Fatalf("reopen: n=%d %s", n, out)
	}
	// invalid text: 422, kept, exit 3 without a terminal
	e.tty = false
	e.edit = func(p string) error {
		return os.WriteFile(p, []byte("source: nope\nwhen: \"true\"\naction: {cmd: [x]}\n"), 0o600)
	}
	if code, _, er := e.do("edit", "rules", "ed"); code != 3 || !strings.Contains(er, "unknown source") {
		t.Fatalf("invalid edit: %d %s", code, er)
	}
}

func TestCLIJobsAndApprove(t *testing.T) {
	e := newCLIEnv(t)
	tx, _ := e.st.DB.Begin()
	now := time.Now()
	id, _ := store.InsertJob(tx, store.Job{Rule: "r1", ActionJSON: "{}", State: "pending_approval", RunAfter: now}, now)
	store.CreateApproval(tx, id, []byte("h"), now.Add(time.Hour))
	tx.Commit()
	if out := e.ok("jobs"); !strings.Contains(out, "pending_approval") {
		t.Fatalf("jobs: %s", out)
	}
	if out := e.ok("jobs", "ls", "-state", "queued"); strings.Contains(out, "pending_approval") {
		t.Fatalf("filter: %s", out)
	}
	if out := e.ok("jobs", "show", "1"); !strings.Contains(out, "rule:") {
		t.Fatalf("show: %s", out)
	}
	if out := e.ok("get", "approvals"); !strings.Contains(out, "r1") {
		t.Fatalf("approvals: %s", out)
	}
	if out := e.ok("approve", "1"); !strings.Contains(out, "approved job 1") {
		t.Fatalf("approve: %s", out)
	}
	if code, _, _ := e.do("deny", "1"); code != 1 { // already decided: 409 from the job endpoint
		if code != 5 {
			t.Fatalf("second decision: %d", code)
		}
	}
	if out := e.ok("get", "audit", "-event", "config_changed"); out == "" {
		t.Fatal("audit")
	}
	if out := e.ok("get", "connections"); !strings.Contains(out, "logins:") {
		t.Fatalf("connections: %s", out)
	}
}

func TestHelpJSONMatchesTable(t *testing.T) {
	var h helpDoc
	var o bytes.Buffer
	c := &cli{in: strings.NewReader(""), out: &o, errw: &bytes.Buffer{}, g: globals{output: "text"}}
	if code := c.run([]string{"help", "--json"}); code != 0 || json.Unmarshal(o.Bytes(), &h) != nil {
		t.Fatalf("help --json: %d %s", code, o.String())
	}
	usage := usageText()
	if len(h.Commands) != len(commands()) || h.Version == "" || len(h.GlobalFlags) != 4 || len(h.ExitCodes) != 6 {
		t.Fatalf("%+v", h)
	}
	for i, k := range commands() {
		d := h.Commands[i]
		if d.Name != k.Name || !strings.Contains(usage, k.Usage) || d.Example == "" || d.Summary == "" || !strings.HasPrefix(d.Usage, "siphon ") {
			t.Errorf("%s: %+v", k.Name, d)
		}
		if (k.build != nil) != (d.Mode == "client") {
			t.Errorf("%s: mode %s", k.Name, d.Mode)
		}
		if k.build != nil {
			// every registered flag is documented, and the reverse
			fs := flag.NewFlagSet(k.Name, flag.ContinueOnError)
			k.build(fs)
			n := 0
			fs.VisitAll(func(f *flag.Flag) {
				n++
				found := false
				for _, fd := range d.Flags {
					found = found || fd.Name == f.Name && fd.Description == f.Usage
				}
				if !found {
					t.Errorf("%s: flag -%s undocumented", k.Name, f.Name)
				}
			})
			if n != len(d.Flags) {
				t.Errorf("%s: %d flags in the table, %d in JSON", k.Name, n, len(d.Flags))
			}
		}
	}
	// -o json works too, and `help <command>` prints the example
	o.Reset()
	if c.g.output = "text"; c.run([]string{"help", "apply"}) != 0 || !strings.Contains(o.String(), "example: siphon apply") {
		t.Fatalf("help apply: %s", o.String())
	}
}

func TestClientModeRule(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
		want bool
	}{
		{"serve", nil, false}, {"validate", nil, false}, {"run-once", nil, false}, {"schema", nil, false}, {"exec-job", nil, false},
		{"config", []string{"export"}, false}, {"credentials", []string{"ls"}, false}, {"rules", []string{"test"}, false},
		{"jobs", []string{"ls", "-config", "x.yaml"}, false}, {"approve", []string{"-config=x.yaml", "3"}, false}, {"jobs", []string{"ls"}, true},
		{"approve", []string{"3"}, true}, {"get", []string{"rules"}, true}, {"status", nil, true}, {"apply", []string{"-config", "x"}, true},
	} {
		if got := clientMode(tc.cmd, tc.args); got != tc.want {
			t.Errorf("%s %v: client=%v, want %v", tc.cmd, tc.args, got, tc.want)
		}
	}
}
