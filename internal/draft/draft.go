// Package draft turns a request in plain words into an apply file by asking a
// model connection, then checks the result and feeds problems back. The
// model's output is untrusted: this package never applies anything.
package draft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/olafkfreund/siphon/docs"
	"github.com/olafkfreund/siphon/internal/applyfile"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/source"
)

// Conn is the resolved model connection. APIKey is only ever sent as a bearer.
type Conn struct {
	Name, BaseURL, APIKey, Model string
}

// Check validates an apply file the way apply's dry-run does: the diff, every
// error, and any warnings. It changes nothing.
type Check func(ctx context.Context, yamlText string) (diff string, errs, warns []string)

type Params struct {
	Request   string
	Conn      Conn
	Inventory map[string]any // names only, never secrets
	Check     Check
	HTTP      *http.Client
	MaxRounds int // default 3
}

type Result struct {
	YAML       string   `json:"yaml"`
	Diff       string   `json:"diff"`
	Errors     []string `json:"errors"`
	Warnings   []string `json:"warnings"`
	Todo       []string `json:"todo"`
	Rounds     int      `json:"rounds"`
	Model      string   `json:"model"`
	Connection string   `json:"connection"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

const roundTimeout = 120 * time.Second

// Run asks the model, checks the answer, and repairs it for up to MaxRounds.
func Run(ctx context.Context, p Params) (*Result, error) {
	if p.MaxRounds == 0 {
		p.MaxRounds = 3
	}
	msgs := Prompt(p.Request, p.Inventory)
	res := &Result{Model: p.Conn.Model, Connection: p.Conn.Name, Errors: []string{}, Warnings: []string{}, Todo: []string{}}
	for round := 1; round <= p.MaxRounds; round++ {
		reply, err := chat(ctx, p.HTTP, p.Conn, msgs)
		if err != nil {
			return nil, err
		}
		y := ExtractYAML(reply)
		diff, errs, warns := p.Check(ctx, y)
		errs = append(errs, approvalErrors(p.Request, y)...)
		res.YAML, res.Diff, res.Errors, res.Warnings, res.Rounds = y, diff, nonNil(errs), nonNil(warns), round
		if len(errs) == 0 {
			break
		}
		msgs = append(msgs, Message{"assistant", reply}, Message{"user",
			"The apply file has these problems:\n- " + strings.Join(errs, "\n- ") + "\nFix only these. Reply with the full corrected apply file in one ```yaml block and nothing else."})
	}
	if items, err := applyfile.Parse([]byte(res.YAML)); err == nil {
		res.Todo = Todo(items, p.Inventory)
	}
	return res, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

var fence = regexp.MustCompile("(?s)```[a-zA-Z]*[ \t]*\r?\n(.*?)```")

// ExtractYAML is the first fenced block of the reply, or the whole reply.
func ExtractYAML(reply string) string {
	if m := fence.FindStringSubmatch(reply); m != nil {
		return strings.TrimSpace(m[1]) + "\n"
	}
	return strings.TrimSpace(reply) + "\n"
}

// approvalErrors catches `approve: false` the request did not ask for.
func approvalErrors(request, y string) []string {
	if strings.Contains(strings.ToLower(request), "approv") || strings.Contains(strings.ToLower(request), "automatic") {
		return nil
	}
	items, err := applyfile.Parse([]byte(y))
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range items {
		var m map[string]any
		if yaml.Unmarshal([]byte(it.YAML), &m) == nil && m["approve"] == false {
			out = append(out, it.Kind+"/"+it.Name+": approve: false was not asked for; remove it (runs keep their approval)")
		}
	}
	return out
}

// ---------------------------------------------------------------- prompt

var stop = map[string]bool{"when": true, "that": true, "with": true, "from": true, "then": true, "this": true, "send": true, "have": true, "into": true, "should": true, "every": true, "each": true, "make": true}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
}

// matchTemplates are the templates that share the most words with the request.
func matchTemplates(request string, n int) []docs.Template {
	type scored struct {
		t docs.Template
		n int
	}
	var all []scored
	for _, t := range docs.Templates() {
		hay := strings.ToLower(t.Name + " " + t.Title + " " + t.Category + " " + t.Notes)
		s := 0
		for _, w := range words(request) {
			if len(w) >= 4 && !stop[w] && strings.Contains(hay, w) {
				s++
			}
		}
		all = append(all, scored{t, s})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].n > all[j].n })
	var out []docs.Template
	for _, s := range all {
		if len(out) < n && s.n > 0 {
			out = append(out, s.t)
		}
	}
	if len(out) == 0 {
		if t, ok := docs.Lookup("webhook-command"); ok {
			out = append(out, t)
		}
	}
	return out
}

func hasAny(request string, keys ...string) bool {
	l := strings.ToLower(request)
	for _, k := range keys {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

func fieldTable(kind string) string {
	_, fields, err := config.Explain(kind)
	if err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s fields\n", kind)
	for _, f := range fields {
		fmt.Fprintf(&b, "- %s (%s%s): %s", f.Path, f.Type, map[bool]string{true: ", required", false: ""}[f.Required], f.Description)
		if len(f.Enum) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(f.Enum, "|"))
		}
		if f.Default != "" {
			fmt.Fprintf(&b, " (default %s)", f.Default)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Prompt is the system and user message for a request. It holds the guide,
// field tables, matching templates and the inventory of names: no secrets.
func Prompt(request string, inv map[string]any) []Message {
	var sys strings.Builder
	sys.WriteString("You write Siphon apply files. Reply with ONE fenced ```yaml block containing the apply file, and nothing else: no explanation.\n")
	sys.WriteString("Sections allowed: sources, agents, routines, credentials (each a map of name to settings) and rules (a list, each with a name). Use names from the inventory for things that exist; create anything else in the same file. Never write secret values: a webhook source needs no secret in the file. Never write approve: false unless the request asks for no approval.\n\n")
	if g, ok := docs.Page("llm"); ok {
		sys.WriteString("## Guide\n" + string(g) + "\n\n")
	}
	sys.WriteString("## Field reference\n")
	kinds := []string{"rule", "source"}
	if hasAny(request, "agent", "review", "summar", "triage", "claude", "model", "llm", "analy", "explain", "investigat", "diagnos") {
		kinds = append(kinds, "agent")
	}
	if hasAny(request, "routine", "steps", "then", "pipeline", "after") {
		kinds = append(kinds, "routine")
	}
	for _, k := range kinds {
		sys.WriteString(fieldTable(k) + "\n")
	}
	sys.WriteString("## Examples\n")
	for _, t := range matchTemplates(request, 3) {
		sys.WriteString("### " + t.Title + "\n```yaml\n" + strings.TrimSpace(t.YAML) + "\n```\n\n")
	}
	invJSON, _ := json.Marshal(inv)
	return []Message{{"system", sys.String()}, {"user", "Existing things (names only): " + string(invJSON) + "\n\nRequest: " + request}}
}

// ---------------------------------------------------------------- model call

type chatResp struct {
	Choices []struct {
		Message struct {
			Content any `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func chat(ctx context.Context, hc *http.Client, c Conn, msgs []Message) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, roundTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": c.Model, "messages": msgs, "temperature": 0.2, "stream": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New("the model call failed (no response from the endpoint)")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the model endpoint answered %d", resp.StatusCode) // never its body: it may echo the key
	}
	var cr chatResp
	if json.Unmarshal(b, &cr) != nil || len(cr.Choices) == 0 {
		return "", errors.New("the model endpoint returned no answer")
	}
	s, _ := cr.Choices[0].Message.Content.(string)
	if strings.TrimSpace(s) == "" {
		return "", errors.New("the model returned an empty answer")
	}
	return s, nil
}

// GuardedClient is an HTTP client for the model endpoint under the same rule
// as listing models: public addresses only, unless the exact host:port is a
// listed private endpoint (never link-local). It dials the checked address
// and follows no redirects. Close the returned func when done.
func GuardedClient(ctx context.Context, cfg *config.Config, rawURL string) (*http.Client, func(), error) {
	host, port, err := config.ModelURL(rawURL)
	if err != nil {
		return nil, nil, errors.New("bad model URL")
	}
	mode := source.Public
	if cfg.PrivateEndpoint(host, port) {
		mode = source.PrivateNoLinkLocal
	}
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	addrs, err := source.ResolveAllowedMode(rctx, host, mode)
	if err != nil {
		return nil, nil, errors.New("this endpoint isn't reachable from Siphon: public addresses only, unless listed in server.models.private_endpoints")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var last error
		for _, a := range addrs {
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), strconv.Itoa(port)))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, tr.CloseIdleConnections, nil
}

// ---------------------------------------------------------------- todo

// WebhookNeedsSecret lists "sources/<name>" for webhook sources the file
// leaves without a secret (the secret is never written into a file).
func WebhookNeedsSecret(items []applyfile.Item) []string {
	var out []string
	for _, it := range items {
		var m map[string]any
		if it.Kind == "sources" && yaml.Unmarshal([]byte(it.YAML), &m) == nil && m["type"] == "webhook" && m["secret"] == nil {
			out = append(out, it.Kind+"/"+it.Name)
		}
	}
	return out
}

func names(inv map[string]any, section string) map[string]bool {
	out := map[string]bool{}
	switch rows := inv[section].(type) {
	case []map[string]any:
		for _, r := range rows {
			out[fmt.Sprint(r["name"])] = true
		}
	case []any:
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok {
				out[fmt.Sprint(m["name"])] = true
			}
		}
	}
	return out
}

// Todo is what the user still has to do for the file to work.
func Todo(items []applyfile.Item, inv map[string]any) []string {
	conns, srcs := names(inv, "connections"), names(inv, "sources")
	defined := map[string]bool{}
	for _, it := range items {
		defined[it.Kind+"/"+it.Name] = true
	}
	var todo []string
	for _, s := range WebhookNeedsSecret(items) {
		todo = append(todo, "secret "+s+".secret: pass it with `siphon apply -f <file> --secret "+s+".secret=-` (`siphon draft --apply` generates one for you)")
	}
	approval := false
	for _, it := range items {
		var m struct {
			Credential string   `yaml:"credential"`
			MCP        []string `yaml:"mcp"`
			Approve    *bool    `yaml:"approve"`
			Action     struct {
				Agent string `yaml:"agent"`
			} `yaml:"action"`
		}
		if yaml.Unmarshal([]byte(it.YAML), &m) != nil {
			continue
		}
		switch it.Kind {
		case "agents":
			if m.Approve == nil || *m.Approve {
				approval = true
			}
			if m.Credential != "" && !conns[m.Credential] && !defined["credentials/"+m.Credential] {
				todo = append(todo, "connection "+m.Credential+" does not exist: `siphon connect model ollama --name "+m.Credential+"` (or `siphon connect login claude --name "+m.Credential+"`)")
			}
			for _, s := range m.MCP {
				if !srcs[s] && !defined["sources/"+s] {
					todo = append(todo, "source "+s+" (an MCP server for agent "+it.Name+") does not exist: connect it, for example `siphon connect github --name "+s+"`")
				}
			}
		case "rules":
			if m.Action.Agent != "" && !defined["agents/"+m.Action.Agent] {
				approval = true // an existing agent: assume it asks first
			}
		}
	}
	if approval {
		todo = append(todo, "agent runs wait for approval: `siphon get approvals`, then `siphon approve <job>`")
	}
	if todo == nil {
		todo = []string{}
	}
	return todo
}
