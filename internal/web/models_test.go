package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Plan #20 step 4: model endpoints are added, tested and listed through the
// portal; private endpoints only when listed; keys never come back.
func TestModelConnections(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen3.8:27b"},{"name":"tiny:1b"}]}`))
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer sk-secret-123" {
				http.Error(w, "no key sk-secret-123", 401)
				return
			}
			w.Write([]byte(`{"data":[{"id":"gpt-oss-20b"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer stub.Close()
	u, _ := url.Parse(stub.URL)
	unlisted := httptest.NewServer(http.NotFoundHandler())
	defer unlisted.Close()
	ce := newCfgEnvFile(t, strings.Replace(cfgFile, "server: { sandbox: none, db: DIR/s.db }",
		`server: { sandbox: none, db: DIR/s.db, models: { private_endpoints: ["`+u.Host+`"] } }`, 1))

	// listed local Ollama: added, tested, listed
	if w := ce.post("/connections/models", url.Values{"name": {"ollama-local"}, "preset": {"ollama"}, "url": {stub.URL}}); w.Code != 303 {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	if c := ce.cur.Load().Credentials["ollama-local"]; c == nil || c.Provider != "ollama" {
		t.Fatal("connection not applied")
	}
	if body := ce.post("/connections/ollama-local/test", nil).Body.String(); !strings.Contains(body, "2 models") || !strings.Contains(body, "tools ✓") {
		t.Fatalf("test: %s", body)
	}
	if body := ce.get("/connections/models-for?f.credential=ollama-local").Body.String(); !strings.Contains(body, `value="qwen3.8:27b"`) {
		t.Fatalf("models-for: %s", body)
	}
	if body := ce.get("/connections").Body.String(); !strings.Contains(body, "ollama-local") || !strings.Contains(body, u.Host) {
		t.Fatal("card missing")
	}

	// OpenAI-compatible with a key: works, and the key never comes back
	if w := ce.post("/connections/models", url.Values{"name": {"oai"}, "preset": {"openai"}, "url": {stub.URL + "/v1"}, "api_key": {"sk-secret-123"}}); w.Code != 303 {
		t.Fatalf("add oai: %d %s", w.Code, w.Body.String())
	}
	test := ce.post("/connections/oai/test", nil).Body.String()
	if !strings.Contains(test, "1 models") {
		t.Fatalf("oai test: %s", test)
	}
	for _, p := range []string{"/connections", "/config/credentials/oai", "/history", "/history/export"} {
		if strings.Contains(ce.get(p).Body.String(), "sk-secret-123") || strings.Contains(test, "sk-secret-123") {
			t.Fatalf("key leaked on %s", p)
		}
	}

	// unlisted private endpoint: refused with the YAML hint, nothing saved
	w := ce.post("/connections/models", url.Values{"name": {"sneaky"}, "preset": {"ollama"}, "url": {unlisted.URL}})
	if w.Code != 422 || !strings.Contains(w.Body.String(), "private_endpoints") || ce.cur.Load().Credentials["sneaky"] != nil {
		t.Fatalf("unlisted private: %d", w.Code)
	}

	// the old path redirects; test needs a session
	if w := ce.get("/logins"); w.Code != 301 || w.Header().Get("Location") != "/connections" {
		t.Fatalf("redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := ce.do("POST", "/connections/ollama-local/test", url.Values{}, nil); w.Code != 401 {
		t.Fatalf("unauthenticated test: %d", w.Code)
	}
}
