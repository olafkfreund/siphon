package web

import (
	"context"
	"encoding/base64"
	"net/url"
	"sort"
	"strings"

	"github.com/olafkfreund/siphon/internal/catalog"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// connTestResult is a connection Test as the API shows it.
type connTestResult struct {
	svcTest
	Connection string `json:"connection"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
}

// connSources lists the sources labelled with a connection, by name.
func connSources(cfg *config.Config, conn string) []string {
	var out []string
	for n, src := range cfg.Sources {
		if src.Connection == conn {
			out = append(out, n)
		}
	}
	if len(out) == 0 { // legacy: unlabelled service sources grouped by name
		for n, src := range cfg.Sources {
			if src.Connection == "" && serviceFor(src) != "" && legacyKey(n) == conn {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// maskParts is every form a token can leak in: whole, bare after a prefix
// ("Bearer ", "token="), and the parts of a Basic credential.
func maskParts(tok string) []string {
	if tok == "" {
		return nil
	}
	parts := []string{tok}
	if i := strings.LastIndexAny(tok, " ="); i >= 0 && i+1 < len(tok) {
		parts = append(parts, tok[i+1:])
	}
	if b, ok := strings.CutPrefix(tok, "Basic "); ok {
		if d, err := base64.StdEncoding.DecodeString(b); err == nil {
			parts = append(parts, string(d))
			parts = append(parts, strings.Split(string(d), ":")...)
		}
	}
	sort.Slice(parts, func(i, j int) bool { return len(parts[i]) > len(parts[j]) })
	return parts
}

// testConnection runs the catalogue entry's test for a connection and stores
// the outcome. The token is read from the connection's own sources, server
// side, and used in memory only.
func (s *server) testConnection(ctx context.Context, conn string) (any, int, error) {
	res, code, err := s.runConnectionTest(ctx, conn)
	if res == nil {
		return nil, code, err
	}
	return res, code, err
}

func (s *server) runConnectionTest(ctx context.Context, conn string) (*connTestResult, int, error) {
	cfg := s.Config()
	names := connSources(cfg, conn)
	if len(names) == 0 {
		return nil, 404, errMsg("no such connection")
	}
	var e *catalog.Entry
	for _, n := range names {
		if svc := serviceFor(cfg.Sources[n]); svc != "" {
			e = catalog.Get(svc)
			break
		}
	}
	if e == nil {
		return nil, 422, errMsg("no test is known for this connection")
	}
	var t svcTest
	var secrets []string
	switch {
	case e.Test != nil:
		token, base := connInputs(cfg, names, e.Test)
		secrets = append(secrets, maskParts(token)...)
		rp := strings.NewReplacer("{token}", token, "{base}", base)
		hdr, val, _ := strings.Cut(rp.Replace(e.Test.Header), ":")
		t = svcTest{Name: conn}
		if token == "" {
			t.Err = "no token is stored for this connection"
		} else {
			t = s.runHTTPTest(ctx, t, orDefault(e.Test.Method, "GET"), testTarget(rp.Replace(e.Test.URL)), strings.TrimSpace(hdr), strings.TrimSpace(val), rp.Replace(e.Test.Body), e.Test.Identity)
		}
	default:
		for _, n := range names {
			if cfg.Sources[n].AWS != "" {
				t = s.testAWS(ctx, n)
				break
			}
		}
		if t.Name == "" {
			return nil, 422, errMsg("this service has nothing to test")
		}
	}
	mask := func(v string) string {
		for _, sec := range secrets {
			if sec != "" {
				v = strings.ReplaceAll(v, sec, "***")
			}
		}
		return v
	}
	t.Err, t.User, t.ARN = mask(t.Err), mask(t.User), mask(t.ARN)
	ok := t.Err == ""
	detail := t.Err
	if ok {
		if detail = firstNonEmpty(t.User, t.ARN); detail == "" {
			detail = "(token accepted)"
		}
	}
	detail = clip(detail, 200)
	if err := store.SetConnectionCheck(s.Store.DB, store.ConnectionCheck{Connection: conn, At: s.Now(), OK: ok, Detail: detail}); err != nil {
		return nil, 500, err
	}
	return &connTestResult{svcTest: t, Connection: conn, OK: ok, Detail: detail}, 200, nil
}

// secretAt reads a secret-bearing value of a source by path: env.NAME,
// headers.NAME or auth.bearer.
func secretAt(src *config.Source, path string) string {
	switch {
	case path == "auth.bearer" && src.Auth != nil:
		return src.Auth.Bearer.Value
	case strings.HasPrefix(path, "env."):
		return src.Env[strings.TrimPrefix(path, "env.")].Value
	case strings.HasPrefix(path, "headers."):
		return src.Headers[strings.TrimPrefix(path, "headers.")].Value
	}
	return ""
}

// connInputs finds the token and the API base of a connection from its own
// sources (never the webhooks'). The test says where (token_from, base_from);
// otherwise the token is the header the test names, a bearer, or an env value,
// and the base is the origin of a service URL (up to /api/ if it has one).
func connInputs(cfg *config.Config, names []string, t *catalog.Test) (token, base string) {
	want, _, _ := strings.Cut(t.Header, ":")
	want = strings.TrimSpace(want)
	for _, n := range names {
		src := cfg.Sources[n]
		if src.Type == "webhook" {
			continue
		}
		if token == "" {
			switch {
			case t.TokenFrom != "":
				token = secretAt(src, t.TokenFrom)
			case src.Headers[want].Value != "":
				token = src.Headers[want].Value
			case src.Auth != nil && src.Auth.Bearer.Value != "":
				token = src.Auth.Bearer.Value
			default:
				ks := make([]string, 0, len(src.Env))
				for k := range src.Env {
					ks = append(ks, k)
				}
				sort.Strings(ks)
				for _, k := range ks {
					if token = src.Env[k].Value; token != "" {
						break
					}
				}
			}
		}
		if base != "" {
			continue
		}
		if t.BaseFrom != "" {
			base = strings.TrimRight(secretAt(src, t.BaseFrom), "/")
		} else if u, err := url.Parse(src.URL); err == nil && u.Host != "" && (src.Type == "http" || src.Type == "mcp") {
			if i := strings.Index(src.URL, "/api/"); i > 0 {
				base = src.URL[:i]
			} else {
				base = u.Scheme + "://" + u.Host
			}
		}
	}
	return
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// testTarget lets a test point a catalogue test at a fake server.
var testTarget = func(u string) string { return u }
