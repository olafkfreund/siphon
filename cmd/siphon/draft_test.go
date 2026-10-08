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

	"github.com/olafkfreund/siphon/internal/draft"
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
	return newCLIEnvFile(t, "server: { sandbox: none, db: DIR/s.db"+server+" }\ncredentials:\n  llm: { provider: openai, url: \""+f.srv.URL+"/v1\", api_key: env:AGW_KEY }\nsources:\n  gh: { type: webhook, secret: env:AGW_HOOKVAL, signature: github }\n  hello-hook: { type: webhook, secret: env:AGW_HOOKVAL, signature: token, token_header: X-Key }\nrules:\n  - { name: r1, source: gh, when: \"true\", action: { cmd: [echo, one] } }\n")
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
	if b, _ := os.ReadFile(filepath.Join(e.dir, "secrets", "sources--drafthook+secret")); string(b) != m[1] {
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

const (
	ghAsk       = "when a GitHub pull request is opened, run a command that prints its title"
	wrongSource = "```yaml\nrules:\n  - name: pr-open\n    source: hello-hook\n    when: 'headers[\"x-github-event\"] == \"pull_request\"'\n    action: { cmd: [echo, opened] }\n```\n"
	rightSource = "```yaml\nrules:\n  - name: pr-open\n    source: github-hooks\n    when: 'headers[\"x-github-event\"] == \"pull_request\"'\n    action: { cmd: [echo, opened] }\n```\n"
)

// The model picks an unrelated webhook for GitHub events: the warning becomes a
// repair, the fixed draft uses github-hooks (not connected yet), and --apply
// refuses until it is.
func TestDraftWrongProviderSourceIsRepaired(t *testing.T) {
	f := newFakeModel(t, wrongSource, rightSource)
	e := draftEnv(t, f, true)
	var res struct {
		YAML         string   `json:"yaml"`
		Diff         string   `json:"diff"`
		Errors       []string `json:"errors"`
		Warnings     []string `json:"warnings"`
		Todo         []string `json:"todo"`
		Placeholders []string `json:"placeholders"`
		Rounds       int      `json:"rounds"`
	}
	if json.Unmarshal([]byte(e.ok("draft", ghAsk, "-o", "json")), &res) != nil {
		t.Fatal("json")
	}
	if res.Rounds != 2 || len(res.Errors) != 0 || !strings.Contains(res.YAML, "source: github-hooks") || strings.Contains(res.YAML, "hello-hook") {
		t.Fatalf("%+v", res)
	}
	// round 2 was told why
	var second struct {
		Messages []struct{ Role, Content string }
	}
	json.Unmarshal([]byte(f.bodies[1]), &second)
	if m := second.Messages[len(second.Messages)-1].Content; !strings.Contains(m, "rules/pr-open: reads GitHub's X-GitHub-Event header but source hello-hook is not a GitHub webhook") || !strings.Contains(m, "github-hooks") {
		t.Fatalf("repair message: %s", m)
	}
	// the prompt names the convention and shows each source's type and signature
	var first struct {
		Messages []struct{ Role, Content string }
	}
	json.Unmarshal([]byte(f.bodies[0]), &first)
	all := first.Messages[0].Content + first.Messages[1].Content
	for _, want := range []string{"github-hooks", "Never reuse an unrelated existing source", `"signature":"token"`, `"signature":"github"`} {
		if !strings.Contains(all, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	// the diff does not offer to create the stand-in; the text output says where it comes from
	if regexp.MustCompile(`(?m)^\+\s+github-hooks:|^\+.*secrets/sources-github`).MatchString(res.Diff) || strings.Contains(res.YAML, "github-hooks:") {
		t.Fatal("the stand-in source shows up as a change")
	}
	txt := e.ok("draft", ghAsk)
	if !strings.Contains(txt, "Changes:\n(github-hooks: placeholder, created by siphon connect github --webhook)\n") || regexp.MustCompile(`(?m)^\+\s+github-hooks:|^\+.*secrets/sources-github`).MatchString(txt) {
		t.Fatalf("text output:\n%s", txt)
	}
	if !strings.Contains(res.Diff, "pr-open") || !regexp.MustCompile(`(?m)^\+.*name: pr-open`).MatchString(res.Diff) {
		t.Fatalf("the rule is missing from the diff:\n%s", res.Diff)
	}
	// the placeholder is reported, with the connect command, and never written into the draft
	if len(res.Placeholders) != 1 || res.Placeholders[0] != "sources/github-hooks" || !strings.Contains(strings.Join(res.Todo, "\n"), "`siphon connect github --webhook` (creates github and github-hooks)") {
		t.Fatalf("placeholders %v todo %v", res.Placeholders, res.Todo)
	}
	// --apply refuses while it is unresolved
	code, _, er := e.do("draft", ghAsk, "--apply", "--yes")
	if code != 3 || !strings.Contains(er, "sources/github-hooks") || !strings.Contains(er, "connect them first") {
		t.Fatalf("apply before connecting: %d %s", code, er)
	}
	if items, _ := store.ConfigItems(e.st.DB); len(items) != 0 {
		t.Fatal("applied with an unresolved placeholder")
	}
	// connect GitHub, then it applies
	e.stdin = "ghp_X\n"
	e.ok("connect", "github", "--token", "-", "--webhook", "--no-test")
	out := e.ok("draft", ghAsk, "--apply", "--yes")
	if !strings.Contains(out, "applied revision") {
		t.Fatalf("after connecting:\n%s", out)
	}
	if y := e.ok("get", "rules", "pr-open"); !strings.Contains(y, "source: github-hooks") {
		t.Fatalf("not applied: %s", y)
	}
}

func TestApplyDryRunWarnsAboutProviderMismatch(t *testing.T) {
	e := draftEnv(t, newFakeModel(t, goodDraft), true)
	f := e.write("w.yaml", "rules:\n  - { name: bad, source: hello-hook, when: 'headers[\"x-github-event\"] == \"push\"', action: { cmd: [echo] } }\n")
	code, _, er := e.do("apply", "-f", f, "--dry-run")
	if code != 0 || !strings.Contains(er, "warning: rules/bad: reads GitHub's X-GitHub-Event header but source hello-hook is not a GitHub webhook") {
		t.Fatalf("%d %s", code, er)
	}
	var j struct{ Warnings []string }
	_, out, _ := e.do("apply", "-f", f, "--dry-run", "-o", "json")
	if json.Unmarshal([]byte(out), &j) != nil || len(j.Warnings) != 1 || !strings.Contains(j.Warnings[0], "github-hooks") {
		t.Fatalf("json warnings: %s", out)
	}
	// a rule on the right source has none, and old warnings are not repeated
	g := e.write("g.yaml", "rules:\n  - { name: good, source: gh, when: 'headers[\"x-github-event\"] == \"push\"', action: { cmd: [echo] } }\n")
	if _, _, er = e.do("apply", "-f", g, "--dry-run"); strings.Contains(er, "warning") {
		t.Fatalf("unexpected warning: %s", er)
	}
}

func TestConnectHints(t *testing.T) {
	for in, want := range map[string]string{
		"github-hooks":    "`siphon connect github --webhook` (creates github and github-hooks)",
		"github":          "`siphon connect github`",
		"corp-gh-hooks":   "`siphon connect aws --name corp-gh --webhook`",
		"gitlab-hooks":    "`siphon connect gitlab --webhook` (creates gitlab and gitlab-hooks)",
		"aws-hooks":       "`siphon connect aws --webhook`",
		"aws-cloudwatch":  "`siphon connect aws --servers cloudwatch`",
		"prod-cloudwatch": "`siphon connect aws --name prod --servers cloudwatch`",
		"github-ci-hooks": "`siphon connect github --name github-ci --webhook`",
	} {
		if got := draft.ConnectHint(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
