package job

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/olafkfreund/siphon/internal/config"
)

const snID = "0123456789abcdef0123456789abcdef"

type snReq struct{ Method, Path, Query, Body string }

// snFake answers every call with reply and records the requests.
type snFake struct {
	*httptest.Server
	mu    sync.Mutex
	reqs  []snReq
	code  int
	reply string
}

func newSNFake(t *testing.T, reply string) *snFake {
	f := &snFake{code: 200, reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"tok","expires_in":1800}`)
			return
		}
		f.reqs = append(f.reqs, snReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		w.WriteHeader(f.code)
		io.WriteString(w, f.reply)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *snFake) last(t *testing.T) snReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no request reached the fake")
	}
	return f.reqs[len(f.reqs)-1]
}

// snConfig has a servicenow source for url; listed puts it in private_endpoints.
func snConfig(t *testing.T, url string, listed bool, auth string) *config.Config {
	t.Helper()
	t.Setenv("SN_URL", url)
	t.Setenv("SN_KEY", "key-secret-value")
	t.Setenv("SN_CID", "cid-1")
	t.Setenv("SN_CSEC", "csec-secret-value")
	eps := ""
	if listed {
		eps = "server: { services: { private_endpoints: [" + strings.TrimPrefix(url, "http://") + "] } }\n"
	}
	cfg, err := config.Parse([]byte(eps + "sources:\n  sn: { type: mcp, package: servicenow, read: { tool: t }, env: { SERVICENOW_INSTANCE_URL: env:SN_URL, " + auth + " } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Validate() // resolves env refs; the servicenow actions are not part of this config
	return cfg
}

const snKeyAuth = "SERVICENOW_API_KEY: env:SN_KEY"

func snRun(cfg *config.Config, a *config.ServiceNowAction, env map[string]any) (int, string, []byte) {
	serviceNowHTTP = func(bool) *http.Client { return http.DefaultClient }
	return runServiceNow(context.Background(), cfg, a, env)
}

func TestRunServiceNow(t *testing.T) {
	old := serviceNowHTTP
	t.Cleanup(func() { serviceNowHTTP = old })
	rec := `{"result":{"sys_id":"` + snID + `","number":"INC0010001","state":"2"}}`
	env := map[string]any{"id": snID, "title": "disk full"}
	cases := []struct {
		name         string
		a            config.ServiceNowAction
		method, path string
		body         string // substring of the request body
		stdout       string // substring of stdout
	}{
		{"work note", config.ServiceNowAction{Connection: "sn", Op: "add_work_note", SysID: "{{.id}}", Fields: map[string]string{"text": "hi {{.title}}"}},
			"PATCH", "/api/now/table/incident/" + snID, `"work_notes":"hi disk full"`, `"number":"INC0010001"`},
		{"comment", config.ServiceNowAction{Connection: "sn", Op: "add_comment", Table: "problem", SysID: snID, Fields: map[string]string{"text": "c"}},
			"PATCH", "/api/now/table/problem/" + snID, `"comments":"c"`, `"table":"problem"`},
		{"create", config.ServiceNowAction{Connection: "sn", Op: "create", Fields: map[string]string{"short_description": "{{.title}}"}},
			"POST", "/api/now/table/incident", `"short_description":"disk full"`, `"sys_id":"` + snID + `"`},
		{"update", config.ServiceNowAction{Connection: "sn", Op: "update", SysID: snID, Fields: map[string]string{"state": "2"}},
			"PATCH", "/api/now/table/incident/" + snID, `"state":"2"`, `"op":"update"`},
		{"get", config.ServiceNowAction{Connection: "sn", Op: "get", SysID: snID, Fields: map[string]string{"fields": "number,state"}},
			"GET", "/api/now/table/incident/" + snID, "", `"state":"2"`},
		{"resolve", config.ServiceNowAction{Connection: "sn", Op: "resolve", SysID: snID, Fields: map[string]string{"close_code": "Solved", "close_notes": "ok"}},
			"PATCH", "/api/now/table/incident/" + snID, `"state":"6"`, `"op":"resolve"`},
		{"create_change", config.ServiceNowAction{Connection: "sn", Op: "create_change", Fields: map[string]string{"type": "normal", "short_description": "x"}},
			"POST", "/api/sn_chg_rest/change/normal", `"short_description":"x"`, `"table":"change_request"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSNFake(t, rec)
			cfg := snConfig(t, f.URL, true, snKeyAuth)
			exit, out, stdout := snRun(cfg, &tc.a, env)
			if exit != 0 {
				t.Fatalf("exit %d: %s", exit, out)
			}
			r := f.last(t)
			if r.Method != tc.method || r.Path != tc.path || !strings.Contains(r.Body, tc.body) {
				t.Fatalf("request %+v", r)
			}
			if tc.name == "get" && !strings.Contains(r.Query, "sysparm_fields=number%2Cstate") {
				t.Fatalf("get query %q", r.Query)
			}
			if !strings.Contains(string(stdout), tc.stdout) || !strings.HasPrefix(out, "servicenow "+tc.a.Op) {
				t.Fatalf("out %q stdout %s", out, stdout)
			}
			if !json.Valid(stdout) {
				t.Fatalf("stdout is not JSON: %s", stdout)
			}
		})
	}
}

func TestRunServiceNowRefusals(t *testing.T) {
	old := serviceNowHTTP
	t.Cleanup(func() { serviceNowHTTP = old })
	env := map[string]any{"id": "x^y"}
	f := newSNFake(t, `{"result":{}}`)
	cfg := snConfig(t, f.URL, true, snKeyAuth)
	note := func(sys string) *config.ServiceNowAction {
		return &config.ServiceNowAction{Connection: "sn", Op: "add_work_note", SysID: sys, Fields: map[string]string{"text": "t"}}
	}
	if exit, out, _ := snRun(cfg, note("{{.id}}"), env); exit != -1 || !strings.Contains(out, "32 hex") {
		t.Fatalf("bad sys_id: %d %q", exit, out)
	}
	if exit, _, _ := snRun(cfg, note("{{.nope}}"), env); exit != -1 {
		t.Fatalf("missing key: %d", exit)
	}
	if len(f.reqs) != 0 {
		t.Fatalf("a refused call reached the instance: %+v", f.reqs)
	}

	// 403: the failure is reported and no secret leaks.
	f.code, f.reply = 403, `{"error":{"message":"denied key-secret-value"}}`
	exit, out, _ := snRun(cfg, note(snID), env)
	if exit != -1 || !strings.Contains(out, "403") || strings.Contains(out, "key-secret-value") {
		t.Fatalf("403: %d %q", exit, out)
	}

	// An instance not in private_endpoints is refused by the guard. serviceNowHTTP
	// is the real guarded client here.
	cfg = snConfig(t, f.URL, false, snKeyAuth)
	serviceNowHTTP = old
	if exit, out, _ := runServiceNow(context.Background(), cfg, note(snID), env); exit != -1 {
		t.Fatalf("unlisted loopback was allowed: %q", out)
	}
}

func TestRunServiceNowOAuth(t *testing.T) {
	old := serviceNowHTTP
	t.Cleanup(func() { serviceNowHTTP = old })
	f := newSNFake(t, `{"result":{"sys_id":"`+snID+`","number":"INC1"}}`)
	cfg := snConfig(t, f.URL, true, "SERVICENOW_CLIENT_ID: env:SN_CID, SERVICENOW_CLIENT_SECRET: env:SN_CSEC")
	a := &config.ServiceNowAction{Connection: "sn", Op: "get", SysID: snID}
	if exit, out, _ := snRun(cfg, a, nil); exit != 0 {
		t.Fatalf("exit %d: %s", exit, out)
	}
}
