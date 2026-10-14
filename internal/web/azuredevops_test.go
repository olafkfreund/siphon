package web

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAzureDevOpsConnectionTest(t *testing.T) {
	const pat = "sekrit-ado-pat"
	b64 := base64.StdEncoding.EncodeToString([]byte(":" + pat))
	var path string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "Basic "+b64 {
			w.WriteHeader(401)
			io.WriteString(w, "bad "+r.Header.Get("Authorization")) // echoes the credential: must be masked
			return
		}
		io.WriteString(w, `{"authenticatedUser":{"providerDisplayName":"Ada"}}`)
	}))
	defer fake.Close()
	fu, _ := url.Parse(fake.URL)
	testTarget = func(u string) string { p, _ := url.Parse(u); return fake.URL + p.RequestURI() }
	defer func() { testTarget = func(u string) string { return u } }()
	cfg := strings.Replace(awsWebCfg, "server: { sandbox: none, db: DIR/s.db,", `server: { sandbox: none, db: DIR/s.db, services: { private_endpoints: ["`+fu.Host+`"] },`, 1)
	ce := newCfgEnvFile(t, strings.Replace(cfg, "mcp_packages: {", "mcp_packages: {"+catalogPackagesYAML(), 1))

	w := ce.api("POST", "/api/services/azure-devops", jbody(map[string]any{"name": "ado", "organization": "my org", "pat": pat}))
	if w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	w = ce.api("POST", "/api/connections/ado/test", "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"user":"Ada"`) || !strings.Contains(body, `"ok":true`) || path != "/my%20org/_apis/connectionData" && path != "/my org/_apis/connectionData" {
		t.Fatalf("test: %d %s (path %q)", w.Code, body, path)
	}
	for _, s := range []string{pat, b64} {
		if strings.Contains(body, s) {
			t.Fatalf("%s in %s", s, body)
		}
	}
}
