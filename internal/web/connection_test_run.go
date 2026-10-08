package web

import (
	"context"
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
	sort.Strings(out)
	return out
}

// testConnection runs the catalogue entry's test for a connection and stores
// the outcome. The token is read from the connection's own sources, server
// side, and used in memory only.
func (s *server) testConnection(ctx context.Context, conn string) (any, int, error) {
	cfg := s.Config()
	names := connSources(cfg, conn)
	if len(names) == 0 {
		return nil, 404, errMsg("no such connection")
	}
	var e *catalog.Entry
	for _, n := range names {
		if svc, _ := serviceOf(cfg.Sources[n]); svc != "" {
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
		token, base := connInputs(cfg, names, e.Test.Header)
		secrets = append(secrets, token)
		rp := strings.NewReplacer("{token}", token, "{base}", base)
		hdr, val, _ := strings.Cut(rp.Replace(e.Test.Header), ":")
		t = svcTest{Name: conn}
		if token == "" {
			t.Err = "no token is stored for this connection"
		} else {
			t = s.runHTTPTest(ctx, t, orDefault(e.Test.Method, "GET"), rp.Replace(e.Test.URL), strings.TrimSpace(hdr), strings.TrimSpace(val), rp.Replace(e.Test.Body), e.Test.Identity)
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
	return connTestResult{svcTest: t, Connection: conn, OK: ok, Detail: detail}, 200, nil
}

// connInputs finds the token and the API base of a connection from its
// sources: the value of the header the test names, a bearer, or an env value;
// the base is a polled URL up to "/api/".
func connInputs(cfg *config.Config, names []string, testHeader string) (token, base string) {
	want, _, _ := strings.Cut(testHeader, ":")
	for _, n := range names {
		src := cfg.Sources[n]
		if src.Type == "webhook" {
			continue
		}
		if token == "" {
			if v := src.Headers[strings.TrimSpace(want)].Value; v != "" {
				token = v
			} else if src.Auth != nil {
				token = src.Auth.Bearer.Value
			}
		}
		if token == "" {
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
		if i := strings.Index(src.URL, "/api/"); base == "" && src.Type == "http" && i > 0 {
			base = src.URL[:i]
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
