package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/catalog"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

// The Services page turns a token and a few choices into ordinary config
// items (sources, a webhook) through the normal validated edit path. Nothing
// it creates is special: every item stays editable on its own page.

const awsHookHeader = "X-Siphon-Key"

const githubMCP = "https://api.githubcopilot.com/mcp/"

// serviceDone is shown once after setup: the webhook secret never again.
type serviceDone = catalog.Done

// serviceOf says which catalogue service made a source, from its shape.
// ponytail: a heuristic for the three first entries; a service id on the item
// would replace it once more entries need Test.
func serviceOf(src *config.Source) (service, detail string) {
	switch {
	case src.Type == "mcp" && (strings.HasPrefix(src.URL, "https://api.githubcopilot.com/") || src.Package == "github"):
		return "github", "MCP tools"
	case src.Type == "webhook" && src.Signature == "github":
		return "github", "webhook"
	case src.Type == "mcp" && (src.AWS != "" || src.Package == "aws-docs"):
		return "aws", "MCP tools"
	case src.Type == "webhook" && src.Signature == "token" && strings.EqualFold(src.TokenHeader, awsHookHeader):
		return "aws", "webhook"
	case src.Type == "http" && strings.Contains(src.URL, "/api/v4/"):
		return "gitlab", "REST polling"
	case src.Type == "webhook" && src.Signature == "token" && strings.EqualFold(src.TokenHeader, "X-Gitlab-Token"):
		return "gitlab", "webhook"
	}
	return "", ""
}

// serviceFor is the catalogue service of a source: its label, else its shape.
func serviceFor(src *config.Source) string {
	if src.Service != "" {
		return src.Service
	}
	svc, _ := serviceOf(src)
	return svc
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

// connect renders a catalogue entry and commits everything it creates as one
// revision, through the same validated edit path as any other change.
func (s *server) connect(actor, id string, values map[string]string) (*serviceDone, error) {
	e := catalog.Get(id)
	if e == nil {
		return nil, errInvalid{"unknown service " + id}
	}
	cfg := s.Config()
	dir := secretsDir(cfg.Server.DB)
	res, err := e.Render(values, catalog.Env{
		Config:     cfg,
		SecretPath: func(kind, name, key string) string { return pendingSecret{Kind: kind, Name: name, Key: key}.path(dir) },
		HookURL:    s.hookURL,
	})
	if err != nil {
		var ue catalog.UserError
		if errors.As(err, &ue) {
			return nil, fieldErr{errInvalid{ue.Msg}, ue.Field}
		}
		return nil, err
	}
	items := make([]store.ConfigItem, len(res.Items))
	for i, it := range res.Items {
		items[i] = store.ConfigItem{Kind: it.Kind, Name: it.Name, YAML: it.YAML}
	}
	pending := make([]pendingSecret, len(res.Secrets))
	for i, sc := range res.Secrets {
		pending[i] = pendingSecret{Kind: sc.Kind, Name: sc.Name, Key: sc.Key, Value: sc.Value}
	}
	done := res.Done
	_, _, applyErr, err := s.commit(actor, "services: "+e.Name+" "+done.Name+" added", nil, putItems(items...), pending)
	if err == nil {
		store.ClearConnectionCheck(s.Store.DB, done.Name) // a reused name starts as Not checked
	}
	if applyErr != nil {
		done.ApplyErr = applyErr.Error()
	}
	return done, err
}

// fieldErr is a refusal that belongs to one form field.
type fieldErr struct {
	errInvalid
	Field string
}

func (e fieldErr) Unwrap() error { return e.errInvalid }

// formValues reads a catalogue entry's fields from a posted form.
func formValues(e *catalog.Entry, r *http.Request) map[string]string {
	v := map[string]string{}
	for _, f := range e.Fields {
		if f.Type == "multi" {
			v[f.Key] = strings.Join(r.PostForm[f.Key], ",")
		} else {
			v[f.Key] = r.PostFormValue(f.Key)
		}
	}
	return v
}

// svcTest is the result of a service Test: who the token belongs to.
type svcTest struct {
	Name      string `json:"name"`
	User      string `json:"user,omitempty"`
	Latency   string `json:"latency,omitempty"`
	Err       string `json:"error,omitempty"`
	ARN       string `json:"arn,omitempty"`        // AWS: caller identity
	Expires   string `json:"expires,omitempty"`    // AWS: when the test keys lapse (UTC, HH:MM)
	ExpiresAt string `json:"expires_at,omitempty"` // the same as RFC 3339, for clients that show local time
	Tools     int    `json:"tools,omitempty"`      // AWS: tools the bridged server lists
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
	var target, hdr, val, who string
	switch {
	case src.Type == "mcp" && src.Auth != nil && strings.HasPrefix(src.URL, "https://api.githubcopilot.com/"):
		target, hdr, val, who = "https://api.github.com/user", "Authorization", "Bearer "+src.Auth.Bearer.Value, "login"
	case src.Type == "mcp" && src.Package == "github":
		target, hdr, val, who = "https://api.github.com/user", "Authorization", "Bearer "+src.Env["GITHUB_PERSONAL_ACCESS_TOKEN"].Value, "login"
	case src.Type == "http" && strings.Contains(src.URL, "/api/v4/"):
		base := src.URL[:strings.Index(src.URL, "/api/v4/")]
		target, hdr, val, who = base+"/api/v4/user", "PRIVATE-TOKEN", src.Headers["PRIVATE-TOKEN"].Value, "username"
	default:
		t.Err = "not a GitHub or GitLab source"
		return t
	}
	return s.runHTTPTest(ctx, t, "GET", target, hdr, val, "", who)
}

// runHTTPTest sends one authenticated request and reports who the token
// belongs to (identity: a dotted JSON path). It runs in the daemon behind the
// same guard as sources: public hosts only, unless listed in
// server.services.private_endpoints. Redirects are not followed. The caller
// owns the secret in val; nothing from the request or body is kept except
// the identity.
func (s *server) runHTTPTest(ctx context.Context, t svcTest, method, target, hdr, val, reqBody, identity string) svcTest {
	cfg := s.Config()
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
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(reqBody))
	if err != nil {
		t.Err = "bad request"
		return t
	}
	req.Header.Set(hdr, val)
	if reqBody != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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
	var doc any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc)
	t.User = jsonPath(doc, identity)
	if t.User == "" {
		t.User = "(token accepted)"
	}
	return t
}

// jsonPath reads a dotted path ("user.login") as a short string, or "".
func jsonPath(v any, path string) string {
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok || k == "" {
			return ""
		}
		v = m[k]
	}
	str, _ := v.(string)
	return clip(str, 100)
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
		t.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	if err != nil {
		t.Err = err.Error()
	}
	return t
}

func (s *server) serviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /services/{name}/test", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		s.render(w, "svctest", s.testService(r.Context(), r.PathValue("name")))
	}))
	s.servicePages(mux)
}
