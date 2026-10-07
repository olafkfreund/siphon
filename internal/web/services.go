package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

// The Services page turns a token and a few choices into ordinary config
// items (sources, a webhook) through the normal validated edit path. Nothing
// it creates is special: every item stays editable on its own page.

const githubMCP = "https://api.githubcopilot.com/mcp/"

// Tool templates offered after setup; names follow github-mcp-server.
var (
	githubReadTools  = []string{"get_me", "get_pull_request", "get_pull_request_files", "get_pull_request_diff", "list_pull_requests", "get_issue", "list_issues", "search_code", "get_file_contents", "list_commits"}
	githubWriteTools = []string{"create_pull_request_review", "add_issue_comment", "create_issue", "update_issue"}
)

// serviceRow is an existing service-made source on the page.
type serviceRow struct {
	Name, Service, Kind, Detail string
}

// serviceDone is shown once after setup: the webhook secret never again.
type serviceDone struct {
	Service, Name, Hook, HookURL, HookSecret, HookHeader string
	ReadTools, WriteTools                                string
}

type serviceForm struct {
	Service, Name, Err string
}

func (s *server) serviceRows() []serviceRow {
	out := []serviceRow{}
	for name, src := range s.Config().Sources {
		r := serviceRow{Name: name, Kind: src.Type}
		switch {
		case src.Type == "mcp" && (strings.HasPrefix(src.URL, "https://api.githubcopilot.com/") || src.Package == "github"):
			r.Service, r.Detail = "github", "MCP tools"
		case src.Type == "webhook" && src.Signature == "github":
			r.Service, r.Detail = "github", "webhook"
		case src.Type == "http" && strings.Contains(src.URL, "/api/v4/"):
			r.Service, r.Detail = "gitlab", "REST polling"
		case src.Type == "webhook" && src.Signature == "token" && strings.EqualFold(src.TokenHeader, "X-Gitlab-Token"):
			r.Service, r.Detail = "gitlab", "webhook"
		default:
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func newSecret(b64 bool) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	if b64 {
		return base64.StdEncoding.EncodeToString(b)
	}
	return hex.EncodeToString(b)
}

func (s *server) hookURL(name string) string {
	if pu := strings.TrimRight(s.Config().Server.PublicURL, "/"); pu != "" {
		return pu + "/hook/" + name
	}
	return "/hook/" + name
}

func putItems(items ...store.ConfigItem) func(map[itemKey]store.ConfigItem) {
	return func(m map[itemKey]store.ConfigItem) {
		for _, it := range items {
			m[itemKey{Kind: it.Kind, Name: it.Name}] = it
		}
	}
}

// addGitHub creates the MCP source (remote or the pinned local package) and,
// optionally, a signed webhook with a generated secret.
func (s *server) addGitHub(actor, name, token, mode string, hook bool) (*serviceDone, error) {
	if !itemName.MatchString(name) || strings.TrimSpace(token) == "" {
		return nil, errInvalid{"a name and a token are required"}
	}
	cfg := s.Config()
	dir := secretsDir(cfg.Server.DB)
	var y string
	var pending []pendingSecret
	switch mode {
	case "local":
		if _, ok := cfg.Server.MCPPackages["github"]; !ok {
			return nil, errInvalid{"the local GitHub MCP server isn't installed: add it to server.mcp_packages (services.siphon.mcpPackages on NixOS)"}
		}
		ps := pendingSecret{Kind: "sources", Name: name, Key: "env.GITHUB_PERSONAL_ACCESS_TOKEN", Value: token}
		y = "type: mcp\npackage: github\nenv:\n  GITHUB_PERSONAL_ACCESS_TOKEN: file:" + ps.path(dir) + "\nread: { tool: get_me }\npoll: 24h\n"
		pending = append(pending, ps)
	default:
		ps := pendingSecret{Kind: "sources", Name: name, Key: "auth.bearer", Value: token}
		y = "type: mcp\nurl: " + githubMCP + "\nauth:\n  bearer: file:" + ps.path(dir) + "\nread: { tool: get_me }\npoll: 24h\n"
		pending = append(pending, ps)
	}
	items := []store.ConfigItem{{Kind: "sources", Name: name, YAML: y}}
	done := &serviceDone{Service: "GitHub", Name: name,
		ReadTools: toolList(name, githubReadTools), WriteTools: toolList(name, append(append([]string{}, githubReadTools...), githubWriteTools...))}
	if hook {
		hn := name + "-hooks"
		secret := newSecret(false)
		ps := pendingSecret{Kind: "sources", Name: hn, Key: "secret", Value: secret}
		items = append(items, store.ConfigItem{Kind: "sources", Name: hn,
			YAML: "type: webhook\nsignature: github\nsecret: file:" + ps.path(dir) + "\nid: header.X-GitHub-Delivery\n"})
		pending = append(pending, ps)
		done.Hook, done.HookURL, done.HookSecret = hn, s.hookURL(hn), secret
	}
	_, _, _, err := s.commit(actor, "services: GitHub "+name+" added", nil, putItems(items...), pending)
	return done, err
}

// addGitLab creates a REST polling source for a project's open merge
// requests and, optionally, a token-authenticated webhook.
func (s *server) addGitLab(actor, name, base, project, token string, hook bool) (*serviceDone, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = "https://gitlab.com"
	}
	if !itemName.MatchString(name) || strings.TrimSpace(token) == "" || strings.TrimSpace(project) == "" {
		return nil, errInvalid{"a name, a project path and a token are required"}
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errInvalid{"base URL must be http(s)://host[:port][/path]"}
	}
	cfg := s.Config()
	dir := secretsDir(cfg.Server.DB)
	ps := pendingSecret{Kind: "sources", Name: name, Key: "headers.PRIVATE-TOKEN", Value: token}
	src := base + "/api/v4/projects/" + url.PathEscape(strings.Trim(project, "/")) + "/merge_requests?state=opened&per_page=20"
	y := fmt.Sprintf("type: http\nurl: %q\nheaders:\n  PRIVATE-TOKEN: file:%s\npoll: 5m\n", src, ps.path(dir))
	if cfg.ServiceEndpoint(base) {
		y += "allow_private: true\n"
	}
	items := []store.ConfigItem{{Kind: "sources", Name: name, YAML: y}}
	pending := []pendingSecret{ps}
	done := &serviceDone{Service: "GitLab", Name: name}
	if hook {
		hn := name + "-hooks"
		secret := newSecret(false)
		hps := pendingSecret{Kind: "sources", Name: hn, Key: "secret", Value: secret}
		items = append(items, store.ConfigItem{Kind: "sources", Name: hn,
			YAML: "type: webhook\nsignature: token\ntoken_header: X-Gitlab-Token\nsecret: file:" + hps.path(dir) + "\nid: header.X-Gitlab-Event-UUID\n"})
		pending = append(pending, hps)
		done.Hook, done.HookURL, done.HookSecret, done.HookHeader = hn, s.hookURL(hn), secret, "X-Gitlab-Token"
	}
	_, _, _, err := s.commit(actor, "services: GitLab "+name+" added", nil, putItems(items...), pending)
	return done, err
}

func toolList(src string, tools []string) string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = "mcp__" + src + "__" + t
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// svcTest is the result of a service Test: who the token belongs to.
type svcTest struct {
	Name, User, Latency, Err string
}

// testService checks a service source's token against the provider's "who
// am I" endpoint. It runs in the daemon behind the same guard as sources:
// public hosts only, unless listed in server.services.private_endpoints. It
// returns the username and latency only; never a body or the token.
func (s *server) testService(ctx context.Context, name string) svcTest {
	t := svcTest{Name: name}
	cfg := s.Config()
	src := cfg.Sources[name]
	if src == nil {
		t.Err = "no such source"
		return t
	}
	var target, hdr, val string
	switch {
	case src.Type == "mcp" && src.Auth != nil && strings.HasPrefix(src.URL, "https://api.githubcopilot.com/"):
		target, hdr, val = "https://api.github.com/user", "Authorization", "Bearer "+src.Auth.Bearer.Value
	case src.Type == "mcp" && src.Package == "github":
		target, hdr, val = "https://api.github.com/user", "Authorization", "Bearer "+src.Env["GITHUB_PERSONAL_ACCESS_TOKEN"].Value
	case src.Type == "http" && strings.Contains(src.URL, "/api/v4/"):
		base := src.URL[:strings.Index(src.URL, "/api/v4/")]
		target, hdr, val = base+"/api/v4/user", "PRIVATE-TOKEN", src.Headers["PRIVATE-TOKEN"].Value
	default:
		t.Err = "not a GitHub or GitLab source"
		return t
	}
	u, err := url.Parse(target)
	if err != nil {
		t.Err = "bad URL"
		return t
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	mode := source.Public
	if cfg.ServiceEndpoint(target) {
		mode = source.PrivateNoLinkLocal
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	addrs, err := source.ResolveAllowedMode(ctx, u.Hostname(), mode)
	if err != nil {
		t.Err = "this host isn't reachable from Siphon: public addresses only, unless listed in server.services.private_endpoints"
		return t
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var last error
		for _, a := range addrs {
			c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), strconv.Itoa(port)))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
	req.Header.Set(hdr, val)
	req.Header.Set("User-Agent", "siphon")
	start := time.Now()
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Err = "no response (" + errClass(err) + ")"
		return t
	}
	defer resp.Body.Close()
	t.Latency = time.Since(start).Round(time.Millisecond).String()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		t.Err = "the token was refused (" + resp.Status + ")"
		return t
	}
	if resp.StatusCode != 200 {
		t.Err = "the service answered " + resp.Status
		return t
	}
	var who struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&who)
	t.User = who.Login + who.Username
	if t.User == "" {
		t.User = "(token accepted)"
	}
	return t
}

func (s *server) serviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /services/github", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		done, err := s.addGitHub("portal", r.PostFormValue("name"), r.PostFormValue("token"), r.PostFormValue("mode"), r.PostFormValue("webhook") != "")
		s.serviceResult(w, r, csrf, "github", r.PostFormValue("name"), done, err)
	}))
	mux.HandleFunc("POST /services/gitlab", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		done, err := s.addGitLab("portal", r.PostFormValue("name"), r.PostFormValue("base"), r.PostFormValue("project"), r.PostFormValue("token"), r.PostFormValue("webhook") != "")
		s.serviceResult(w, r, csrf, "gitlab", r.PostFormValue("name"), done, err)
	}))
	mux.HandleFunc("POST /services/{name}/test", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		s.render(w, "svctest", s.testService(r.Context(), r.PathValue("name")))
	}))
}

// serviceResult shows the one-time result page, or the form with the error.
func (s *server) serviceResult(w http.ResponseWriter, r *http.Request, csrf, service, name string, done *serviceDone, err error) {
	var inv errInvalid
	if errors.As(err, &inv) {
		v := view{CSRF: csrf, Services: s.serviceRows(), ServiceForm: &serviceForm{Service: service, Name: name, Err: inv.msg}}
		s.pageStatus(w, r, "services", v, http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the webhook secret is on this page
	s.page(w, r, "servicedone", view{CSRF: csrf, ServiceDone: done})
}
