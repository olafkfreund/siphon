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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
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
	Service    string `json:"service"`
	Name       string `json:"name"`
	Hook       string `json:"hook,omitempty"`
	HookURL    string `json:"hook_url,omitempty"`
	HookSecret string `json:"hook_secret,omitempty"`
	HookHeader string `json:"hook_header,omitempty"`
	ReadTools  string `json:"read_tools,omitempty"`
	WriteTools string `json:"write_tools,omitempty"`
	ApplyErr   string `json:"apply_error,omitempty"` // the save worked but applying it live failed
}

type serviceForm struct {
	Service, Name, Err string
	AWSCloudWatch      bool     // server.mcp_packages has aws-cloudwatch
	AWSDocs            bool     // ... aws-docs
	Profiles, RoleARNs []string // server.aws allowlists: what a portal credential may use without its own keys
}

func (s *server) serviceForm() *serviceForm {
	pk := s.Config().Server.MCPPackages
	_, cw := pk["aws-cloudwatch"]
	_, docs := pk["aws-docs"]
	aws := s.Config().Server.AWS
	return &serviceForm{AWSCloudWatch: cw, AWSDocs: docs, Profiles: aws.Profiles, RoleARNs: aws.RoleARNs}
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
		case src.Type == "mcp" && (src.AWS != "" || src.Package == "aws-docs"):
			r.Service, r.Detail = "aws", "MCP tools"
		case src.Type == "webhook" && src.Signature == "token" && strings.EqualFold(src.TokenHeader, awsHookHeader):
			r.Service, r.Detail = "aws", "webhook"
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

// freeNames refuses a name whose source, or its -hooks twin, already exists:
// creating it would silently replace the item (and its secret).
func freeNames(cfg *config.Config, name string) error {
	for _, n := range []string{name, name + "-hooks"} {
		if cfg.Sources[n] != nil {
			return errInvalid{"a source named " + n + " already exists; pick another name or edit it"}
		}
	}
	return nil
}

func loopbackHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return h == "localhost"
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
	if err := freeNames(cfg, name); err != nil {
		return nil, err
	}
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
	_, _, applyErr, err := s.commit(actor, "services: GitHub "+name+" added", nil, putItems(items...), pending)
	if applyErr != nil {
		done.ApplyErr = applyErr.Error()
	}
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
	if u, _ := url.Parse(base); u.Scheme != "https" && !loopbackHost(u.Hostname()) && !cfg.ServiceEndpoint(base) {
		return nil, errInvalid{"the base URL must be https (http only for localhost or a host listed in server.services.private_endpoints): the token would travel in clear"}
	}
	if err := freeNames(cfg, name); err != nil {
		return nil, err
	}
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
	_, _, applyErr, err := s.commit(actor, "services: GitLab "+name+" added", nil, putItems(items...), pending)
	if applyErr != nil {
		done.ApplyErr = applyErr.Error()
	}
	return done, err
}

const awsHookHeader = "X-Siphon-Key"

var awsName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

// awsServers maps the form's server choice to the package, source suffix and label.
var awsServers = []struct{ Key, Package, Suffix string }{
	{"cloudwatch", "aws-cloudwatch", "-cloudwatch"},
	{"docs", "aws-docs", "-docs"},
}

// addAWS creates an aws credential, one source per chosen server and,
// optionally, a webhook (EventBridge API destination with an API key header).
// Base keys (role mode only) are stored write-only like other tokens.
func (s *server) addAWS(actor, name, region, mode, profile, roleARN, externalID, keyID, secretKey string, servers []string, hook bool) (*serviceDone, error) {
	name, region = strings.TrimSpace(name), strings.TrimSpace(region)
	profile, roleARN, externalID = strings.TrimSpace(profile), strings.TrimSpace(roleARN), strings.TrimSpace(externalID)
	if !awsName.MatchString(name) || region == "" {
		return nil, errInvalid{"a lowercase name (a-z, 0-9, - _) and a region are required"}
	}
	cfg := s.Config()
	var chosen []struct{ Key, Package, Suffix string }
	for _, sv := range awsServers {
		if !slices.Contains(servers, sv.Key) {
			continue
		}
		if _, ok := cfg.Server.MCPPackages[sv.Package]; !ok {
			return nil, errInvalid{"the " + sv.Key + " server isn't installed: enable services.siphon.aws in your NixOS config"}
		}
		chosen = append(chosen, sv)
	}
	if len(chosen) == 0 {
		return nil, errInvalid{"choose at least one server"}
	}
	if cfg.Credentials[name] != nil {
		return nil, errInvalid{"a credential named " + name + " already exists; pick another name"}
	}
	for _, sv := range chosen {
		if err := freeNames(cfg, name+sv.Suffix); err != nil {
			return nil, err
		}
	}
	if err := freeNames(cfg, name); hook && err != nil {
		return nil, err
	}
	dir := secretsDir(cfg.Server.DB)
	y := fmt.Sprintf("provider: aws\nregion: %q\n", region)
	var pending []pendingSecret
	switch mode {
	case "role":
		if roleARN == "" {
			return nil, errInvalid{"a role ARN is required"}
		}
		y += fmt.Sprintf("role_arn: %q\n", roleARN)
		if externalID != "" {
			y += fmt.Sprintf("external_id: %q\n", externalID)
		}
		if (keyID == "") != (secretKey == "") {
			return nil, errInvalid{"base access keys go together: give both or neither"}
		}
		if keyID != "" {
			ak := pendingSecret{Kind: "credentials", Name: name, Key: "access_key_id", Value: keyID}
			sk := pendingSecret{Kind: "credentials", Name: name, Key: "secret_access_key", Value: secretKey}
			y += "access_key_id: file:" + ak.path(dir) + "\nsecret_access_key: file:" + sk.path(dir) + "\n"
			pending = append(pending, ak, sk)
		}
	default:
		if profile == "" {
			return nil, errInvalid{"a profile name is required"}
		}
		y += fmt.Sprintf("profile: %q\n", profile)
	}
	items := []store.ConfigItem{{Kind: "credentials", Name: name, YAML: y}}
	done := &serviceDone{Service: "AWS", Name: name}
	var tools []string
	for _, sv := range chosen {
		sn := name + sv.Suffix
		sy := "type: mcp\npackage: " + sv.Package + "\n"
		if sv.Key == "cloudwatch" {
			sy += "aws: " + name + "\n"
		}
		items = append(items, store.ConfigItem{Kind: "sources", Name: sn, YAML: sy})
		tools = append(tools, "mcp__"+sn)
	}
	done.ReadTools = "[" + strings.Join(tools, ", ") + "]"
	if hook {
		hn := name + "-hooks"
		secret := newSecret(false)
		ps := pendingSecret{Kind: "sources", Name: hn, Key: "secret", Value: secret}
		items = append(items, store.ConfigItem{Kind: "sources", Name: hn,
			YAML: "type: webhook\nsignature: token\ntoken_header: " + awsHookHeader + "\nsecret: file:" + ps.path(dir) + "\n"})
		pending = append(pending, ps)
		done.Hook, done.HookURL, done.HookSecret, done.HookHeader = hn, s.hookURL(hn), secret, awsHookHeader
	}
	_, _, applyErr, err := s.commit(actor, "services: AWS "+name+" added", nil, putItems(items...), pending)
	if applyErr != nil {
		done.ApplyErr = applyErr.Error()
	}
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
	Name    string `json:"name"`
	User    string `json:"user,omitempty"`
	Latency string `json:"latency,omitempty"`
	Err     string `json:"error,omitempty"`
	ARN     string `json:"arn,omitempty"`     // AWS: caller identity
	Expires string `json:"expires,omitempty"` // AWS: when the test keys lapse
	Tools   int    `json:"tools,omitempty"`   // AWS: tools the bridged server lists
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
	if src.Type == "mcp" && src.AWS != "" {
		return s.testAWS(ctx, name)
	}
	if src.Type == "mcp" && strings.HasPrefix(src.Package, "aws-") {
		t.Err = "nothing to test: this server needs no credentials"
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

func (s *server) testAWS(ctx context.Context, name string) svcTest {
	t := svcTest{Name: name}
	if s.TestAWS == nil {
		t.Err = "AWS tests are not available here"
		return t
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	arn, exp, tools, err := s.TestAWS(ctx, name)
	t.ARN, t.Tools = arn, len(tools)
	if !exp.IsZero() {
		t.Expires = exp.UTC().Format("15:04 MST")
	}
	if err != nil {
		t.Err = err.Error()
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
	mux.HandleFunc("POST /services/aws", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		done, err := s.addAWS("portal", r.PostFormValue("name"), r.PostFormValue("region"), r.PostFormValue("mode"), r.PostFormValue("profile"),
			r.PostFormValue("role_arn"), r.PostFormValue("external_id"), r.PostFormValue("access_key_id"), r.PostFormValue("secret_access_key"),
			r.PostForm["servers"], r.PostFormValue("webhook") != "")
		s.serviceResult(w, r, csrf, "aws", r.PostFormValue("name"), done, err)
	}))
	mux.HandleFunc("POST /services/{name}/test", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		s.render(w, "svctest", s.testService(r.Context(), r.PathValue("name")))
	}))
}

// serviceResult shows the one-time result page, or the form with the error.
func (s *server) serviceResult(w http.ResponseWriter, r *http.Request, csrf, service, name string, done *serviceDone, err error) {
	var inv errInvalid
	if errors.As(err, &inv) {
		v := view{CSRF: csrf, Services: s.serviceRows(), ServiceForm: s.serviceFormErr(service, name, inv.msg)}
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

func (s *server) serviceFormErr(service, name, msg string) *serviceForm {
	f := s.serviceForm()
	f.Service, f.Name, f.Err = service, name, msg
	return f
}
