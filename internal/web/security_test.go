package web

import (
	"encoding/json"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/olafkfreund/siphon/internal/store"
)

// secFile has items an overlay may keep but not newly set.
const secFile = `server: { sandbox: none, db: DIR/s.db }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
  stdio: { type: mcp, command: [srv, --x], read: { resource: "test://v" }, poll: 1m, allow_private: true }
agents:
  noegress: { kind: claude, prompt: p, egress: { enabled: false } }
rules:
  - { name: r1, source: gh, when: "true", action: { cmd: [echo, one] } }
`

func newSecEnv(t *testing.T) *cfgEnv { return newCfgEnvFile(t, secFile) }

func (ce *cfgEnv) apiPut(kind, name, y string) (int, string) {
	b, _ := json.Marshal(map[string]any{"yaml": y})
	w := ce.api("PUT", "/api/config/"+kind+"/"+name, string(b))
	return w.Code, w.Body.String()
}

// reject: the HTML form path and the API answer 422 with want in the
// message, and nothing is stored, applied or revised.
func (ce *cfgEnv) reject(t *testing.T, kind, name, y, want string) {
	t.Helper()
	revs, applied := ce.latest(), ce.applied.Load()
	w := ce.saveYAML(kind, name, y)
	if body := html.UnescapeString(w.Body.String()); w.Code != 422 || !strings.Contains(body, want) {
		t.Fatalf("%s/%s HTML: %d, want 422 with %q\n%s", kind, name, w.Code, want, body)
	}
	if code, body := ce.apiPut(kind, name, y); code != 422 || !strings.Contains(body, want) {
		t.Fatalf("%s/%s API: %d %s, want 422 with %q", kind, name, code, body, want)
	}
	if ce.latest() != revs || ce.applied.Load() != applied {
		t.Fatalf("%s/%s: a rejected item changed state", kind, name)
	}
	if items, _ := store.ConfigItems(ce.st.DB); len(items) != ce.keep {
		t.Fatalf("%s/%s: a rejected item was stored", kind, name)
	}
}

// accept: both paths store it.
func (ce *cfgEnv) accept(t *testing.T, kind, name, y string) {
	t.Helper()
	if w := ce.saveYAML(kind, name, y); w.Code != 303 {
		t.Fatalf("%s/%s HTML: %d\n%s", kind, name, w.Code, html.UnescapeString(w.Body.String()))
	}
	if code, body := ce.apiPut(kind, name, y); code != 200 {
		t.Fatalf("%s/%s API: %d %s", kind, name, code, body)
	}
	ce.keep++
}

const (
	msgCmd  = "command (stdio MCP) can only be set in siphon.yaml"
	msgRef  = "secret refs can only point at this item's stored secrets or keep the value from siphon.yaml"
	msgPriv = "allow_private can only be set in siphon.yaml"
	msgEgr  = "egress.enabled: false can only be set in siphon.yaml"
)

func TestSecurityF1StdioCommand(t *testing.T) {
	ce := newSecEnv(t)
	ce.reject(t, "sources", "evil", `{type: mcp, command: [sh, -c, id], read: {resource: x}, poll: 1m}`, msgCmd)
	ce.reject(t, "sources", "stdio", `{type: mcp, command: [sh, -c, id], read: {resource: "test://v"}, poll: 1m, allow_private: true}`, msgCmd)
	// The file's own command may stay while the rest of the item changes.
	ce.accept(t, "sources", "stdio", `{type: mcp, command: [srv, --x], read: {resource: "test://v"}, poll: 2m, allow_private: true}`)
	for _, f := range kindFields["sources"] {
		if f.Key == "command" || f.Key == "allow_private" {
			t.Fatalf("the source form must not offer %s", f.Key)
		}
	}
	// A form save keeps the file's command and cannot change it.
	w := ce.post("/config/sources/stdio/save", url.Values{"mode": {"form"}, "name": {"stdio"}, "rev": {strconv.FormatInt(ce.latest(), 10)},
		"f.type": {"mcp"}, "f.read.resource": {"test://v"}, "f.poll": {"3m"}, "f.command": {"sh\n-c\nid"}})
	if w.Code != 303 || strings.Join(ce.cur.Load().Sources["stdio"].Command, " ") != "srv --x" {
		t.Fatalf("form changed the command: %d %v", w.Code, ce.cur.Load().Sources["stdio"].Command)
	}
}

func TestSecurityF2Refs(t *testing.T) {
	ce := newSecEnv(t)
	sec := filepath.Join(ce.dir, "secrets")
	for _, bad := range []string{"env:HOME", "file:/etc/passwd", "file:" + sec + "/../../etc/passwd", "file:" + sec + "/sources-other-secret",
		"file:" + sec + "/other-evil-secret", "file:" + filepath.Join(ce.dir, "sources-evil-secret")} {
		ce.reject(t, "sources", "evil", `{type: webhook, secret: "`+bad+`", signature: github}`, msgRef)
		ce.reject(t, "sources", "evil", `{type: http, url: "https://example.com", poll: 1m, auth: {bearer: "`+bad+`"}}`, msgRef)
		ce.reject(t, "sources", "evil", `{type: http, url: "https://example.com", poll: 1m, headers: {X-Key: "`+bad+`"}}`, msgRef)
		ce.reject(t, "credentials", "evil", `{provider: claude, api_key: "`+bad+`"}`, msgRef)
		ce.reject(t, "agents", "evil", `{kind: claude, prompt: p, api_key_file: "`+strings.TrimPrefix(bad, "file:")+`"}`, msgRef)
	}
	if code, body := ce.apiPut("sources", "evil", `{type: webhook, secret: "file:/etc/passwd", signature: github}`); strings.Contains(body, "passwd") || code != 422 {
		t.Fatalf("message echoes the ref: %s", body)
	}
	// A ref into this item's own secrets that cannot be read: no hint about the OS error.
	missing := "file:" + sec + "/sources--own+secret"
	if code, body := ce.apiPut("sources", "own", `{type: webhook, secret: "`+missing+`", signature: github}`); code != 422 || strings.Contains(body, "no such file") {
		t.Fatalf("missing own secret: %d %s", code, body)
	}
	// This item's own stored secret, and the file's own env ref, are fine.
	os.MkdirAll(sec, 0o700)
	os.WriteFile(filepath.Join(sec, "sources--own+secret"), []byte("v"), 0o600)
	ce.accept(t, "sources", "own", `{type: webhook, secret: "`+missing+`", signature: github}`)
	ce.accept(t, "sources", "gh", `{type: webhook, secret: env:AGW_HOOK, signature: sha256, signature_header: X-Sig}`)
	// ...but another item may not borrow them.
	ce.reject(t, "sources", "borrower", `{type: webhook, secret: "`+missing+`", signature: github}`, msgRef)
	ce.reject(t, "sources", "borrower", `{type: webhook, secret: env:AGW_HOOK, signature: github}`, msgRef)
}

func TestSecurityF3AllowPrivate(t *testing.T) {
	ce := newSecEnv(t)
	ce.reject(t, "sources", "ssrf", `{type: http, url: "http://169.254.169.254/", poll: 1m, allow_private: true}`, msgPriv)
	ce.reject(t, "sources", "gh", `{type: webhook, secret: env:AGW_HOOK, signature: github, allow_private: true}`, msgPriv)
	ce.accept(t, "sources", "stdio", `{type: mcp, command: [srv, --x], read: {resource: "test://v"}, poll: 1m, allow_private: true}`) // the file already has it
	mod, _ := os.ReadFile(filepath.Join("..", "..", "nix", "module.nix"))
	if !strings.Contains(string(mod), "IPAddressDeny = metadataDeny;\n        # Hardening. Actions run") {
		t.Fatal("siphon.service does not deny the metadata addresses")
	}
}

func TestSecurityF4AnchorsAndAliases(t *testing.T) {
	ce := newCfgEnvFile(t, strings.Replace(secFile, "sources:\n", "limits: &L { agent_runs_per_day: 5 }\nsources:\n", 1))
	const want = "anchor" // "anchors and aliases are not allowed", or an alias to an anchor no item defines
	ce.reject(t, "sources", "evil", `{type: http, url: "https://example.com", poll: 1m, headers: &L {A: b}}`, want)
	ce.reject(t, "sources", "evil", `{type: http, url: "https://example.com", poll: 1m, headers: *L}`, want)
	ce.reject(t, "agents", "evil", "kind: claude\nprompt: &L hi\n", want)
	if ce.cur.Load().Limits.AgentRunsPerDay != 5 {
		t.Fatal("limits changed")
	}
}

func TestSecurityF5SecretWrites(t *testing.T) {
	ce := newSecEnv(t)
	sec := filepath.Join(ce.dir, "secrets")
	form := func(rev int64, typ string) url.Values {
		return url.Values{"mode": {"form"}, "name": {"w1"}, "rev": {strconv.FormatInt(rev, 10)}, "f.type": {typ}, "f.signature": {"github"}, "f.secret": {"PASTED-VALUE"}}
	}
	noFiles := func(why string) {
		t.Helper()
		if es, err := os.ReadDir(sec); err == nil && len(es) > 0 {
			t.Fatalf("%s wrote a secret file: %v", why, es)
		}
	}
	// Review writes nothing, and the directory does not even appear.
	if w := ce.post("/config/sources/new/check", form(0, "webhook")); w.Code != 200 {
		t.Fatalf("check: %d", w.Code)
	}
	if _, err := os.Stat(sec); !os.IsNotExist(err) {
		t.Fatal("check created the secrets dir")
	}
	// A stale save and an invalid save write nothing either.
	if w := ce.post("/config/sources/new/save", form(99, "webhook")); w.Code != 409 {
		t.Fatalf("stale: %d", w.Code)
	}
	noFiles("a stale save")
	if w := ce.post("/config/sources/new/save", form(0, "bogus")); w.Code != 422 {
		t.Fatalf("invalid: %d", w.Code)
	}
	noFiles("an invalid save")
	// A good save: 0600 file, 0700 plain dir, no temp files left behind.
	if w := ce.post("/config/sources/new/save", form(0, "webhook")); w.Code != 303 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	fi, err := os.Lstat(filepath.Join(sec, "sources--w1+secret"))
	if err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("secret file: %v %v", fi, err)
	}
	if di, _ := os.Lstat(sec); di.Mode()&os.ModeSymlink != 0 || di.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir: %v", di.Mode())
	}
	if es, _ := os.ReadDir(sec); len(es) != 1 {
		t.Fatalf("leftovers: %v", es)
	}
	if got := ce.cur.Load().Sources["w1"].Secret.Value; got != "PASTED-VALUE" {
		t.Fatalf("applied config has %q, not the stored value", got)
	}
}

func TestSecuritySecretsDirSymlinkRefused(t *testing.T) {
	ce := newSecEnv(t)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(ce.dir, "secrets")); err != nil {
		t.Fatal(err)
	}
	w := ce.post("/config/sources/new/save", url.Values{"mode": {"form"}, "name": {"w2"}, "rev": {"0"}, "f.type": {"webhook"}, "f.signature": {"github"}, "f.secret": {"PASTED"}})
	if w.Code < 400 {
		t.Fatalf("symlinked secrets dir accepted: %d", w.Code)
	}
	if es, _ := os.ReadDir(target); len(es) != 0 {
		t.Fatalf("wrote through the symlink: %v", es)
	}
	if items, _ := store.ConfigItems(ce.st.DB); len(items) != 0 {
		t.Fatal("item stored although its secret could not be")
	}
	// The write helper refuses on its own, and refuses names that could leave the dir.
	if err := writeSecret(filepath.Join(ce.dir, "secrets"), pendingSecret{Kind: "sources", Name: "x", Key: "secret", Value: "v"}); err == nil {
		t.Fatal("writeSecret followed a symlinked dir")
	}
	for _, n := range []string{"../x", "a/b", "", ".hidden"} {
		if err := writeSecret(t.TempDir(), pendingSecret{Kind: "sources", Name: n, Key: "secret", Value: "v"}); err == nil {
			t.Errorf("writeSecret accepted name %q", n)
		}
	}
}

func TestSecurityF8EgressDisable(t *testing.T) {
	ce := newSecEnv(t)
	ce.reject(t, "agents", "open", `{kind: claude, prompt: p, egress: {enabled: false}}`, msgEgr)
	ce.accept(t, "agents", "noegress", `{kind: claude, prompt: changed, egress: {enabled: false}}`) // the file already has it off
	ce.accept(t, "agents", "more", `{kind: claude, prompt: p, egress: {allow: [x.example.com]}}`)   // allowing a host is the intended path
	if a := ce.cur.Load().Agents["more"]; a == nil || len(a.Egress.Allow) != 1 {
		t.Fatalf("allow not applied: %+v", a)
	}
}
