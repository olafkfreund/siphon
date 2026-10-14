package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServiceNowConnectionTest(t *testing.T) {
	var deny atomic.Bool
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth_token.do":
			io.WriteString(w, `{"access_token":"sekrit-token","expires_in":1800}`)
		case deny.Load():
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"denied"}}`)
		case r.URL.Path == "/api/now/table/sys_user":
			io.WriteString(w, `{"result":[{"user_name":"svc.siphon"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	u, _ := url.Parse(fake.URL)
	// The fake is on loopback, so it must be listed: the guard stays as it is.
	ce := newCfgEnvFile(t, strings.Replace(awsWebCfg, "server: { sandbox: none, db: DIR/s.db,",
		`server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["`+u.Host+`"] },`, 1))

	w := ce.api("POST", "/api/services/servicenow", jbody(map[string]any{"name": "snow", "instance": fake.URL,
		"client_id": "sekrit-id", "client_secret": "sekrit-secret"}))
	if w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	w = ce.api("POST", "/api/connections/snow/test", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"svc.siphon"`) || !strings.Contains(w.Body.String(), `"detail":"svc.siphon"`) || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}

	deny.Store(true)
	w = ce.api("POST", "/api/connections/snow/test", "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "401") || !strings.Contains(body, "denied") {
		t.Fatalf("denied: %d %s", w.Code, body)
	}
	for _, s := range []string{"sekrit-id", "sekrit-secret", "sekrit-token"} {
		if strings.Contains(body, s) {
			t.Fatalf("%s in %s", s, body)
		}
	}
}
