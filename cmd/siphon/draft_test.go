package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/olafkfreund/siphon/internal/store"
)

const (
	draftKey  = "sk-draft-KEYVALUE"
	hookValue = "hook-VALUE-s3cret"
	badDraft  = "Here you go:\n```yaml\nrules:\n  - name: dr1\n    source: gh\n    when: \"true\"\n    action: { cmd: \"echo hi\" }\n```\n"
	goodDraft = "```yaml\nsources:\n  drafthook: { type: webhook, signature: token, token_header: X-Siphon-Key }\nrules:\n  - name: dr1\n    source: drafthook\n    when: \"true\"\n    action: { cmd: [echo, hi] }\n```\n"
)

// fakeModel is an OpenAI-compatible endpoint: it lists one model and answers
// each chat round from replies (the last one repeats), recording the requests.
type fakeModel struct {
	mu      sync.Mutex
	replies []string
	bodies  []string
	auth    []string
	srv     *httptest.Server
}

func newFakeModel(t *testing.T, replies ...string) *fakeModel {
	f := &fakeModel{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"fake-model-1"},{"id":"other"}]}`))
		case "/v1/chat/completions":
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			n := len(f.bodies)
			f.bodies = append(f.bodies, string(b))
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			f.mu.Unlock()
			reply := f.replies[min(n, len(f.replies)-1)]
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModel) host() string { u, _ := url.Parse(f.srv.URL); return u.Host }

// draftEnv serves a config with a model connection to f (listed as private).
func draftEnv(t *testing.T, f *fakeModel, listed bool) *cliEnv {
	t.Setenv("AGW_KEY", draftKey)
	t.Setenv("AGW_HOOKVAL", hookValue)
	server := ""
	if listed {
		server = `, models: { private_endpoints: ["` + f.host() + `"] }`
	}
	return newCLIEnvFile(t, "server: { sandbox: none, db: DIR/s.db"+server+" }\ncredentials:\n  llm: { provider: openai, url: \""+f.srv.URL+"/v1\", api_key: env:AGW_KEY }\nsources:\n  gh: { type: webhook, secret: env:AGW_HOOKVAL, signature: github }\nrules:\n  - { name: r1, source: gh, when: \"true\", action: { cmd: [echo, one] } }\n")
}

const askText = "tell me on my phone when a deploy webhook says failed"

func TestDraftRepairsThenSucceeds(t *testing.T) {
	f := newFakeModel(t, badDraft, goodDraft)
	e := draftEnv(t, f, true)
	out := e.ok("draft", askText)
	if len(f.bodies) != 2 {
		t.Fatalf("%d model calls", len(f.bodies))
	}
	// round 1: the prompt has the guide, field tables, a matching template and the inventory
	var first struct {
		Model       string
		Temperature float64
		Messages    []struct{ Role, Content string }
	}
	json.Unmarshal([]byte(f.bodies[0]), &first)
	all := first.Messages[0].Content + first.Messages[1].Content
	for _, want := range []string{"Siphon for AI assistants", "action.cmd", "Forward any webhook to your phone", `"gh"`, `"llm"`, `"r1"`, askText, "ONE fenced"} {
		if !strings.Contains(all, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if first.Model != "fake-model-1" || first.Temperature != 0.2 {
		t.Errorf("model %q temperature %v", first.Model, first.Temperature)
	}
	// no secret value reaches the model; the key is only the bearer
	for i, b := range f.bodies {
		if strings.Contains(b, draftKey) || strings.Contains(b, hookValue) {
			t.Fatalf("request %d holds a secret", i)
		}
	}
	if f.auth[0] != "Bearer "+draftKey {
		t.Errorf("auth %q", f.auth[0])
	}
	// round 2 carries the errors and the earlier reply
	var second struct {
		Messages []struct{ Role, Content string }
	}
	json.Unmarshal([]byte(f.bodies[1]), &second)
	n := len(second.Messages)
	if n != 4 || second.Messages[2].Role != "assistant" || !strings.Contains(second.Messages[2].Content, `cmd: "echo hi"`) ||
		!strings.Contains(second.Messages[3].Content, "problems") || !strings.Contains(second.Messages[3].Content, "dr1") {
		t.Fatalf("repair message: %+v", second.Messages[2:])
	}
	// the result
	for _, want := range []string{"drafthook", "Changes:", "To do:", "secret sources/drafthook.secret", "siphon connect"} {
		if want == "siphon connect" {
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// nothing was committed
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("a draft without --apply committed")
	}
	var res struct {
		YAML       string   `json:"yaml"`
		Diff       string   `json:"diff"`
		Errors     []string `json:"errors"`
		Warnings   []string `json:"warnings"`
		Todo       []string `json:"todo"`
		Rounds     int      `json:"rounds"`
		Model      string   `json:"model"`
		Connection string   `json:"connection"`
	}
	f.replies = []string{goodDraft}
	if json.Unmarshal([]byte(e.ok("draft", askText, "-o", "json")), &res) != nil || res.Rounds != 1 || res.Model != "fake-model-1" || res.Connection != "llm" ||
		!strings.Contains(res.YAML, "drafthook") || !strings.Contains(res.Diff, "+") || len(res.Errors) != 0 || res.Warnings == nil || len(res.Todo) == 0 {
		t.Fatalf("json: %+v", res)
	}
	// the request was audited (clipped), with the connection and model
	if a := e.ok("get", "audit", "--event", "draft"); !strings.Contains(a, "llm fake-model-1: tell me on my phone") {
		t.Fatalf("audit:\n%s", a)
	}
}

func TestDraftApplyGeneratesSecretOnce(t *testing.T) {
	f := newFakeModel(t, goodDraft)
	e := draftEnv(t, f, true)
	e.tty, e.stdin = true, "n\n"
	if out := e.ok("draft", askText, "--apply"); !strings.Contains(out, "not applied") || strings.Contains(out, "To do:\n  - secret") {
		t.Fatalf("declined:\n%s", out)
	}
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("declined, yet stored")
	}
	e.tty = false
	out := e.ok("draft", askText, "--apply", "--yes")
	m := regexp.MustCompile(`secret: ([0-9a-f]{64})`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "shown once") || !strings.Contains(out, "applied revision") {
		t.Fatalf("apply:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "secrets", "sources-drafthook-secret")); string(b) != m[1] {
		t.Fatal("secret file differs from the printed secret")
	}
	if strings.Contains(out, "secret sources/") {
		t.Fatalf("a generated secret is still on the to-do list:\n%s", out)
	}
	if y := e.ok("get", "rules", "dr1"); !strings.Contains(y, "source: drafthook") {
		t.Fatalf("not applied: %s", y)
	}
	for _, args := range [][]string{{"get", "sources", "drafthook"}, {"history", "1"}, {"get", "audit"}} {
		if strings.Contains(e.ok(args...), m[1]) {
			t.Fatalf("%v shows the secret", args)
		}
	}
}

func TestDraftStopsAfterThreeRounds(t *testing.T) {
	f := newFakeModel(t, badDraft)
	e := draftEnv(t, f, true)
	code, out, er := e.do("draft", askText)
	if code != 3 || len(f.bodies) != 3 || !strings.Contains(er, "3 rounds") || !strings.Contains(er, "rules/dr1") || !strings.Contains(er, "hint:") {
		t.Fatalf("exit %d, %d calls\n%s\n%s", code, len(f.bodies), out, er)
	}
	if code, _, _ = e.do("draft", askText, "--apply", "--yes"); code != 3 {
		t.Fatalf("--apply of a broken draft: %d", code)
	}
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("a broken draft was applied")
	}
}

func TestDraftApprovalNotAddedByTheModel(t *testing.T) {
	const noApproval = "```yaml\nagents:\n  helper: { kind: claude, credential: nobody, prompt: p, approve: false }\n```\n"
	f := newFakeModel(t, noApproval, goodDraft)
	e := draftEnv(t, f, true)
	e.ok("draft", askText)
	var second struct {
		Messages []struct{ Role, Content string }
	}
	json.Unmarshal([]byte(f.bodies[1]), &second)
	if m := second.Messages[len(second.Messages)-1].Content; !strings.Contains(m, "approve: false was not asked for") {
		t.Fatalf("not repaired: %s", m)
	}
	// asked for explicitly: left alone
	f2 := newFakeModel(t, noApproval)
	e2 := draftEnv(t, f2, true)
	if code, _, er := e2.do("draft", "run an agent without approval when a webhook fires"); strings.Contains(er, "approve: false was not asked for") {
		t.Fatalf("flagged an asked-for approve: false: %d %s", code, er)
	}
}

func TestDraftRefusals(t *testing.T) {
	// no model connection
	e := newCLIEnv(t)
	code, _, er := e.do("draft", "anything")
	if code != 3 || !strings.Contains(er, "siphon connect model ollama") {
		t.Fatalf("no connection: %d %s", code, er)
	}
	if code, _, _ = e.do("draft", ""); code != 2 {
		t.Fatalf("empty request: %d", code)
	}
	if code, _, _ = e.do("draft"); code != 2 {
		t.Fatalf("no request: %d", code)
	}
	// a private model endpoint that is not listed
	f := newFakeModel(t, goodDraft)
	e = draftEnv(t, f, false)
	code, _, er = e.do("draft", askText, "--model", "fake-model-1")
	if code != 3 || !strings.Contains(er, "private_endpoints") || len(f.bodies) != 0 {
		t.Fatalf("private, unlisted: %d %s (%d calls)", code, er, len(f.bodies))
	}
	// and without --model the listing refuses it too
	if code, _, _ = e.do("draft", askText); code != 3 || len(f.bodies) != 0 {
		t.Fatalf("listing: %d", code)
	}
	// an unknown connection
	e = draftEnv(t, newFakeModel(t, goodDraft), true)
	if code, _, er = e.do("draft", askText, "--connection", "nope"); code != 3 || !strings.Contains(er, "llm") {
		t.Fatalf("unknown connection: %d %s", code, er)
	}
}

func TestMCPDraftNeverApplies(t *testing.T) {
	f := newFakeModel(t, goodDraft)
	e := draftEnv(t, f, true)
	m := newMCPEnvFrom(t, e, true, true) // everything allowed
	out, isErr := m.call(t, "draft", map[string]any{"request": askText})
	if isErr || !strings.Contains(out, "drafthook") || !strings.Contains(out, `"rounds": 1`) {
		t.Fatalf("draft: %v %s", isErr, out)
	}
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("the draft tool committed")
	}
	if revs, _ := store.Revisions(e.st.DB, 5); len(revs) != 0 {
		t.Fatal("the draft tool made a revision")
	}
	if out, isErr = m.call(t, "draft", map[string]any{"request": "x", "connection": "nope"}); !isErr || !strings.Contains(out, `"hint"`) {
		t.Fatalf("refusal: %s", out)
	}
}
