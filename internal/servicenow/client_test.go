package servicenow

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSecret = "sekrit-client-secret"
	testKey    = "sekrit-api-key"
	testToken  = "sekrit-token"
	sysID      = "0123456789abcdef0123456789abcdef"
)

func oauthClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, Auth{ClientID: "id", ClientSecret: testSecret}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func noSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, s := range []string{testSecret, testKey, testToken} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaks %q: %v", s, err)
		}
	}
}

func TestTokenCachedAcrossCalls(t *testing.T) {
	var tokens, calls atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			tokens.Add(1)
			io.WriteString(w, `{"access_token":"`+testToken+`","expires_in":1800}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		calls.Add(1)
		io.WriteString(w, `{"result":{"number":"INC0001"}}`)
	}))
	for range 2 {
		if _, err := c.Get(context.Background(), "incident", sysID, nil); err != nil {
			t.Fatal(err)
		}
	}
	if tokens.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("tokens=%d calls=%d", tokens.Load(), calls.Load())
	}
}

func TestTokenRefreshedWhenExpired(t *testing.T) {
	var tokens atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			tokens.Add(1)
			io.WriteString(w, `{"access_token":"`+testToken+`","expires_in":100}`)
			return
		}
		io.WriteString(w, `{"result":{}}`)
	}))
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	c.Get(context.Background(), "incident", sysID, nil)
	now = now.Add(30 * time.Second) // inside expires_in-60
	c.Get(context.Background(), "incident", sysID, nil)
	now = now.Add(20 * time.Second) // past it
	c.Get(context.Background(), "incident", sysID, nil)
	if tokens.Load() != 2 {
		t.Fatalf("tokens=%d, want 2", tokens.Load())
	}
}

func TestUnauthorizedRefreshesOnce(t *testing.T) {
	var tokens, calls atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			tokens.Add(1)
			io.WriteString(w, `{"access_token":"`+testToken+`","expires_in":1800}`)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"User Not Authenticated"}}`)
			return
		}
		io.WriteString(w, `{"result":{"ok":"yes"}}`)
	}))
	if _, err := c.Get(context.Background(), "incident", sysID, nil); err != nil {
		t.Fatal(err)
	}
	if tokens.Load() != 2 || calls.Load() != 2 {
		t.Fatalf("tokens=%d calls=%d", tokens.Load(), calls.Load())
	}
}

func TestUnauthorizedTwiceErrorsWithoutLoop(t *testing.T) {
	var calls atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"`+testToken+`","expires_in":1800}`)
			return
		}
		calls.Add(1)
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"User Not Authenticated"}}`)
	}))
	_, err := c.Get(context.Background(), "incident", sysID, nil)
	noSecrets(t, err)
	if calls.Load() != 2 {
		t.Fatalf("calls=%d, want 2", calls.Load())
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "User Not Authenticated") {
		t.Fatalf("error: %v", err)
	}
}

func TestAPIKeySkipsOAuth(t *testing.T) {
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			t.Error("oauth_token.do called with an API key")
		}
		if r.Header.Get("x-sn-apikey") != testKey || r.Header.Get("Authorization") != "" {
			t.Errorf("headers: %v", r.Header)
		}
		io.WriteString(w, `{"result":{}}`)
	}))
	c.auth = Auth{APIKey: testKey}
	if _, err := c.Get(context.Background(), "incident", sysID, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHTMLIsUnavailable(t *testing.T) {
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html>Instance Hibernating</html>`)
	}))
	_, err := c.Get(context.Background(), "incident", sysID, nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func Test429RetriesAfterSleep(t *testing.T) {
	var calls atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		io.WriteString(w, `{"result":{}}`)
	}))
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.Get(context.Background(), "incident", sysID, nil); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second || calls.Load() != 2 {
		t.Fatalf("slept=%v calls=%d", slept, calls.Load())
	}
}

func Test429TwiceErrors(t *testing.T) {
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		w.Header().Set("Retry-After", "99") // clamped to 30 s
		w.WriteHeader(429)
	}))
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	_, err := c.Get(context.Background(), "incident", sysID, nil)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err=%v", err)
	}
	if len(slept) != 1 || slept[0] != 30*time.Second {
		t.Fatalf("slept=%v", slept)
	}
}

func TestRedirectIsErrorAndNotFollowed(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer target.Close()
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	_, err := c.Get(context.Background(), "incident", sysID, nil)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("err=%v", err)
	}
	if hit.Load() {
		t.Fatal("redirect was followed")
	}
}

func TestReadFieldsMaySaySys(t *testing.T) {
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		if r.URL.Query().Get("sysparm_fields") != "sys_id,number" {
			t.Errorf("query: %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"result":[]}`)
	}))
	if _, err := c.Query(context.Background(), "incident", "", []string{"sys_id", "number"}, 5); err != nil {
		t.Fatal(err)
	}
}

func TestInputsRejectedBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	ctx := context.Background()
	for name, err := range map[string]error{
		"table caps":      second(c.Get(ctx, "Incident", sysID, nil)),
		"table slash":     second(c.Get(ctx, "incident/../x", sysID, nil)),
		"table long":      second(c.Get(ctx, "a"+strings.Repeat("b", 80), sysID, nil)),
		"sys_id short":    second(c.Get(ctx, "incident", "abc", nil)),
		"sys_id caps":     second(c.Get(ctx, "incident", strings.ToUpper(sysID), nil)),
		"read field":      second(c.Get(ctx, "incident", sysID, []string{"number", "Bad"})),
		"query table":     second(c.Query(ctx, "x y", "", nil, 5)),
		"query field":     second(c.Query(ctx, "incident", "", []string{"Bad"}, 5)),
		"create field":    second(c.Create(ctx, "incident", map[string]any{"sys_id": "x"})),
		"create field 2":  second(c.Create(ctx, "incident", map[string]any{"a-b": "x"})),
		"patch sys_id":    second(c.Patch(ctx, "incident", "../x", map[string]any{"state": "6"})),
		"patch field":     second(c.Patch(ctx, "incident", sysID, map[string]any{"sys_updated_by": "x"})),
		"change kind":     second(c.CreateChange(ctx, "bogus", nil)),
		"change field":    second(c.CreateChange(ctx, "normal", map[string]any{"sys_id": "x"})),
		"change template": second(c.CreateChange(ctx, "standard", map[string]any{"standard_change_template_id": "../x"})),
		"change no tmpl":  second(c.CreateChange(ctx, "standard", map[string]any{"short_description": "x"})),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests sent", hits.Load())
	}
}

func second[T any](_ T, err error) error { return err }

func TestQueryUsesEncodedValuesAndClampsLimit(t *testing.T) {
	var got string
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		got = r.URL.RawQuery
		q := r.URL.Query()
		if q.Get("sysparm_query") != "number=INC1^active=true" || q.Get("sysparm_limit") != "50" ||
			q.Get("sysparm_display_value") != "true" || q.Get("sysparm_exclude_reference_link") != "true" ||
			q.Get("sysparm_fields") != "number,state" {
			t.Errorf("query: %v", q)
		}
		io.WriteString(w, `{"result":[{"number":"INC1"}]}`)
	}))
	rows, err := c.Query(context.Background(), "incident", "number=INC1^active=true", []string{"number", "state"}, 500)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if strings.Contains(got, "^") {
		t.Fatalf("query not escaped: %s", got)
	}
}

func TestWritesAndChange(t *testing.T) {
	type seen struct{ method, path, body string }
	var log []seen
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		log = append(log, seen{r.Method, r.URL.Path, string(b)})
		io.WriteString(w, `{"result":{"number":"X1"}}`)
	}))
	ctx := context.Background()
	c.Create(ctx, "incident", map[string]any{"short_description": "x"})
	c.Patch(ctx, "incident", sysID, map[string]any{"work_notes": "hi"})
	c.CreateChange(ctx, "normal", map[string]any{"short_description": "c"})
	c.CreateChange(ctx, "standard", map[string]any{"standard_change_template_id": sysID})
	want := []seen{
		{"POST", "/api/now/table/incident", `{"short_description":"x"}`},
		{"PATCH", "/api/now/table/incident/" + sysID, `{"work_notes":"hi"}`},
		{"POST", "/api/sn_chg_rest/change/normal", `{"short_description":"c"}`},
		{"POST", "/api/sn_chg_rest/change/standard/" + sysID, `{"standard_change_template_id":"` + sysID + `"}`},
	}
	if len(log) != len(want) {
		t.Fatalf("log=%v", log)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Errorf("%d: got %v want %v", i, log[i], want[i])
		}
	}
}

func TestWhoami(t *testing.T) {
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		q := r.URL.Query()
		if r.URL.Path != "/api/now/table/sys_user" || q.Get("sysparm_query") != "sys_id=javascript:gs.getUserID()" ||
			q.Get("sysparm_fields") != "user_name" || q.Get("sysparm_limit") != "1" {
			t.Errorf("request: %s", r.URL)
		}
		io.WriteString(w, `{"result":[{"user_name":"svc.siphon"}]}`)
	}))
	u, err := c.Whoami(context.Background())
	if err != nil || u != "svc.siphon" {
		t.Fatalf("u=%q err=%v", u, err)
	}
}

func TestErrorsNeverLeakSecrets(t *testing.T) {
	// The server echoes every secret back; the client must not repeat them.
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_client","error_description":"`+testSecret+`"}`)
			return
		}
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"message":"denied"}}`)
	}))
	_, err := c.Get(context.Background(), "incident", sysID, nil)
	noSecrets(t, err)
	c.auth = Auth{APIKey: testKey}
	_, err = c.Get(context.Background(), "incident", sysID, nil)
	noSecrets(t, err)
	if !strings.Contains(err.Error(), "servicenow: GET /api/now/table/incident/"+sysID+": 403 denied") {
		t.Fatalf("error: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	oauth := Auth{ClientID: "i", ClientSecret: "s"}
	for _, tc := range []struct {
		base string
		auth Auth
		ok   bool
	}{
		{"https://dev1.service-now.com", oauth, true},
		{"https://dev1.service-now.com", Auth{APIKey: "k"}, true},
		{"http://127.0.0.1:8080", oauth, true},
		{"http://dev1.service-now.com", oauth, false},
		{"https://dev1.service-now.com/path", oauth, false},
		{"https://u:p@dev1.service-now.com", oauth, false},
		{"https://dev1.service-now.com?x=1", oauth, false},
		{"https://dev1.service-now.com", Auth{ClientID: "i"}, false},
		{"https://dev1.service-now.com", Auth{}, false},
		{"", oauth, false},
	} {
		_, err := New(tc.base, tc.auth, http.DefaultClient)
		if (err == nil) != tc.ok {
			t.Errorf("New(%q, %+v): err=%v", tc.base, tc.auth, err)
		}
	}
}

func TestClientFromEnv(t *testing.T) {
	env := map[string]string{"SERVICENOW_INSTANCE_URL": "https://dev1.service-now.com", "SERVICENOW_CLIENT_ID": "i", "SERVICENOW_CLIENT_SECRET": testSecret}
	get := func(k string) string { return env[k] }
	if _, err := ClientFromEnv(get, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	delete(env, "SERVICENOW_INSTANCE_URL")
	_, err := ClientFromEnv(get, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "SERVICENOW_INSTANCE_URL") {
		t.Fatalf("err=%v", err)
	}
	env["SERVICENOW_INSTANCE_URL"] = "https://dev1.service-now.com"
	env["SERVICENOW_API_KEY"] = testKey // API key wins
	delete(env, "SERVICENOW_CLIENT_ID")
	if _, err := ClientFromEnv(get, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
}

func TestTokenWaiterGivesUpOnContext(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
	}))
	defer close(release)
	go c.Get(context.Background(), "incident", sysID, nil) // holds the token lock
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Get(ctx, "incident", sysID, nil); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter blocked behind the token fetch")
	}
}

func TestShortLivedTokenUsesHalfLifetime(t *testing.T) {
	var tokens atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			tokens.Add(1)
			io.WriteString(w, `{"access_token":"t","expires_in":30}`)
			return
		}
		io.WriteString(w, `{"result":{}}`)
	}))
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	c.Get(context.Background(), "incident", sysID, nil)
	now = now.Add(10 * time.Second)
	c.Get(context.Background(), "incident", sysID, nil)
	if tokens.Load() != 1 {
		t.Fatalf("tokens=%d, want 1", tokens.Load())
	}
	now = now.Add(10 * time.Second) // 20 s: past 15 s
	c.Get(context.Background(), "incident", sysID, nil)
	if tokens.Load() != 2 {
		t.Fatalf("tokens=%d, want 2", tokens.Load())
	}
}

func TestMissingExpiresInIsNotCached(t *testing.T) {
	var tokens atomic.Int32
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			tokens.Add(1)
			io.WriteString(w, `{"access_token":"t"}`)
			return
		}
		io.WriteString(w, `{"result":{}}`)
	}))
	c.Get(context.Background(), "incident", sysID, nil)
	c.Get(context.Background(), "incident", sysID, nil)
	if tokens.Load() != 2 {
		t.Fatalf("tokens=%d, want 2", tokens.Load())
	}
}

func TestJSONWithoutResultIsUnavailable(t *testing.T) {
	c, _ := oauthClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth_token.do" {
			io.WriteString(w, `{"access_token":"t","expires_in":1800}`)
			return
		}
		io.WriteString(w, `{"status":"maintenance"}`)
	}))
	if _, err := c.Get(context.Background(), "incident", sysID, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}
