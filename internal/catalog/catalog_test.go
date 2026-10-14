package catalog

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEmbeddedCatalogueLoads(t *testing.T) {
	for _, id := range []string{"github", "gitlab", "aws"} {
		if Get(id) == nil {
			t.Errorf("%s missing", id)
		}
	}
}

const testCat = `services:
  - id: demo
    name: Demo
    mark: DM
    category: generic
    summary: s
    status: available
    fields:
      - {key: name, label: Name, type: text, default: demo, required: true}
      - {key: email, label: Email, type: text, required: true}
      - {key: token, label: Token, type: secret, required: true}
    creates:
      - kind: sources
        name: '{{.name}}'
        when: 'true'
        yaml: |
          type: http
          url: https://example.com/{{.name}}
          headers:
            Authorization: {{basic "email" "token" "headers.Authorization"}}
`

func TestBasicIsASecretAndNeverInYAML(t *testing.T) {
	es, err := Load([]byte(testCat))
	if err != nil {
		t.Fatal(err)
	}
	res, err := es[0].Render(map[string]string{"email": "a@b.c", "token": "SENTINEL-TOKEN"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Secrets) != 1 || res.Secrets[0].Value != "Basic YUBiLmM6U0VOVElORUwtVE9LRU4=" {
		t.Fatalf("secrets %+v", res.Secrets)
	}
	y := res.Items[0].YAML
	if strings.Contains(y, "SENTINEL") || strings.Contains(y, "YUBi") || !strings.Contains(y, "file:") || !strings.Contains(y, "connection: \"demo\"\nservice: demo\n") {
		t.Fatalf("yaml:\n%s", y)
	}
	// A newline in a non-secret value can't smuggle YAML.
	if _, err := es[0].Render(map[string]string{"email": "a\nb: c", "token": "x"}, Env{}); err == nil {
		t.Fatal("multi-line value accepted")
	} else if ue := (UserError{}); !errors.As(err, &ue) {
		t.Fatalf("not a user error: %v", err)
	}
}

// nix/catalog-packages.json is generated: go generate ./internal/catalog
func TestNixPackagesJSONIsFresh(t *testing.T) {
	want, err := NixPackagesJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../nix/catalog-packages.json")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("nix/catalog-packages.json is stale (%v); run: go generate ./internal/catalog", err)
	}
}

func TestSecretAndGeneratedPrefix(t *testing.T) {
	es, err := Load([]byte(`services:
  - id: demo
    name: Demo
    mark: DM
    category: generic
    summary: s
    status: available
    fields:
      - {key: name, label: Name, type: text, default: demo, required: true}
      - {key: token, label: Token, type: secret, required: true}
    creates:
      - kind: sources
        name: '{{.name}}'
        when: 'true'
        yaml: |
          type: http
          url: https://example.com
          headers:
            Authorization: {{secret "token" "headers.Authorization" "Bearer "}}
      - kind: sources
        name: '{{.name}}-hook'
        when: 'true'
        yaml: |
          type: webhook
          secret: {{generated "secret" "Bearer "}}
`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := es[0].Render(map[string]string{"token": "abc"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Secrets[0].Value != "Bearer abc" || !strings.HasPrefix(res.Secrets[1].Value, "Bearer ") || res.Done.HookSecret != strings.TrimPrefix(res.Secrets[1].Value, "Bearer ") {
		t.Fatalf("%+v %+v", res.Secrets, res.Done)
	}
}

func TestServiceNowEntry(t *testing.T) {
	e := Get("servicenow")
	if e == nil {
		t.Fatal("servicenow missing")
	}
	res, err := e.Render(map[string]string{"name": "snow"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Kind != "sources" || res.Items[0].Name != "snow-hooks" || !strings.Contains(res.Items[0].YAML, "signature: standard-webhooks") {
		t.Fatalf("%+v", res.Items)
	}
	// The secret itself is stored write-only (the YAML holds a file ref), as in
	// TestSecretAndGeneratedPrefix.
	if len(res.Secrets) != 1 || !regexp.MustCompile(`^whsec_[0-9a-f]{64}$`).MatchString(res.Secrets[0].Value) {
		t.Fatalf("secret not whsec_ + 64 hex: %d secrets", len(res.Secrets))
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(res.Done.HookSecret) || res.Secrets[0].Value != "whsec_"+res.Done.HookSecret {
		t.Fatal("Done.HookSecret is not the 64-hex key behind the stored secret")
	}
	if res.Done.Hook != "snow-hooks" {
		t.Fatalf("Done.Hook = %q", res.Done.Hook)
	}
	// Agent tools: the optional MCP source, by authentication mode.
	const inst = "https://dev1.service-now.com"
	for _, tc := range []struct {
		name string
		in   map[string]string
		keys []string // env keys of the "snow" source; nil: an error
		none []string // env keys that must be absent
	}{
		{"oauth", map[string]string{"instance": inst, "client_id": "cid", "client_secret": "csec"},
			[]string{"SERVICENOW_INSTANCE_URL", "SERVICENOW_CLIENT_ID", "SERVICENOW_CLIENT_SECRET"}, []string{"SERVICENOW_API_KEY"}},
		{"apikey", map[string]string{"instance": inst, "auth": "apikey", "api_key": "k"},
			[]string{"SERVICENOW_INSTANCE_URL", "SERVICENOW_API_KEY"}, []string{"SERVICENOW_CLIENT_ID", "SERVICENOW_CLIENT_SECRET"}},
		{"oauth without secret", map[string]string{"instance": inst, "client_id": "cid"}, nil, nil},
		{"apikey without key", map[string]string{"instance": inst, "auth": "apikey"}, nil, nil},
	} {
		in := map[string]string{"name": "snow"}
		for k, v := range tc.in {
			in[k] = v
		}
		res, err := e.Render(in, Env{})
		if tc.keys == nil {
			if err == nil {
				t.Errorf("%s: no error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(res.Items) != 2 || res.Items[0].Name != "snow" || res.Items[1].Name != "snow-hooks" {
			t.Errorf("%s: items %+v", tc.name, res.Items)
			continue
		}
		y := res.Items[0].YAML
		if !strings.Contains(y, "package: servicenow") || strings.Contains(y, "dev1.service-now.com") || strings.Contains(y, "csec") {
			t.Errorf("%s: yaml %s", tc.name, y)
		}
		for _, k := range tc.keys {
			if !strings.Contains(y, k+": ") {
				t.Errorf("%s: missing %s in %s", tc.name, k, y)
			}
		}
		for _, k := range tc.none {
			if strings.Contains(y, k) {
				t.Errorf("%s: unexpected %s", tc.name, k)
			}
		}
	}
}

func TestAzureEventGridEntry(t *testing.T) {
	e := Get("azure-eventgrid")
	if e == nil || e.Status != "available" {
		t.Fatalf("entry: %+v", e)
	}
	res, err := e.Render(map[string]string{"name": "eg"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	y := res.Items[0].YAML
	if len(res.Items) != 1 || res.Items[0].Name != "eg-hooks" || !strings.Contains(y, "signature: token") || !strings.Contains(y, "token_header: X-Siphon-Key") || !strings.Contains(y, "format: eventgrid") {
		t.Fatalf("%+v", res.Items)
	}
	if len(res.Secrets) != 1 || res.Secrets[0].Value == "" || !strings.Contains(e.Setup, "http_max_body") {
		t.Fatalf("secrets %+v", res.Secrets)
	}
}

func TestMattermostEntry(t *testing.T) {
	e := Get("mattermost")
	if e == nil {
		t.Fatal("mattermost missing")
	}
	res, err := e.Render(map[string]string{"name": "mm", "token": "tok", "reply": "Working on it"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Name != "mm-hooks" || !strings.Contains(res.Items[0].YAML, "signature: body-token") || !strings.Contains(res.Items[0].YAML, `reply_text: "Working on it"`) {
		t.Fatalf("%+v", res.Items)
	}
	if len(res.Secrets) != 1 || res.Secrets[0].Value != "tok" || strings.Contains(res.Items[0].YAML, "tok\n") {
		t.Fatalf("secrets %+v", res.Secrets)
	}
	var v map[string]any
	if err := yaml.Unmarshal([]byte(res.Items[0].YAML), &v); err != nil || v["reply_text"] != "Working on it" {
		t.Fatalf("yaml: %v %v", err, v)
	}
	res, err = e.Render(map[string]string{"name": "mm", "token": "tok"}, Env{})
	if err != nil || strings.Contains(res.Items[0].YAML, "reply_text") {
		t.Fatalf("no reply: %v %s", err, res.Items[0].YAML)
	}
	res, err = Get("webhook").Render(map[string]string{"name": "w", "mode": "body-token"}, Env{})
	if err != nil || !strings.Contains(res.Items[0].YAML, "signature: body-token") || strings.Contains(res.Items[0].YAML, "_header") {
		t.Fatalf("generic body-token: %v %+v", err, res)
	}
}

func TestDiscordEntry(t *testing.T) {
	e := Get("discord")
	if e == nil || e.Status == "not-yet" {
		t.Fatal("discord missing or not available")
	}
	key := strings.Repeat("ab", 32)
	res, err := e.Render(map[string]string{"name": "dc", "public_key": key, "reply": "Working on it"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	y := res.Items[0].YAML
	if res.Items[0].Name != "dc-hooks" || !strings.Contains(y, "signature: ed25519") || !strings.Contains(y, "format: discord") || !strings.Contains(y, `reply_text: "Working on it"`) {
		t.Fatalf("%+v", res.Items)
	}
	if len(res.Secrets) != 1 || res.Secrets[0].Value != key {
		t.Fatalf("secrets %+v", res.Secrets)
	}
	if _, err := e.Render(map[string]string{"name": "dc", "public_key": "not-a-key"}, Env{}); err == nil {
		t.Fatal("a bad public key was accepted")
	}
}

func TestNotionEntry(t *testing.T) {
	e := Get("notion")
	if e == nil || e.Status != "available" {
		t.Fatalf("entry: %+v", e)
	}
	res, err := e.Render(map[string]string{"name": "nt", "webhook": "on"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.Items[0].Name != "nt" || res.Items[1].Name != "nt-hooks" {
		t.Fatalf("%+v", res.Items)
	}
	if m, h := res.Items[0].YAML, res.Items[1].YAML; !strings.Contains(m, "url: https://mcp.notion.com/mcp") || !strings.Contains(m, "oauth: {}") ||
		!strings.Contains(h, "format: notion") || !strings.Contains(h, "signature_header: X-Notion-Signature") || !strings.Contains(h, `signature_prefix: "sha256="`) || strings.Contains(h, "secret:") {
		t.Fatalf("yaml: %s\n%s", m, h)
	}
	if len(res.Secrets) != 0 || !strings.Contains(e.Setup, "not yet verified") && !strings.Contains(e.Setup, "Not yet verified") {
		t.Fatalf("secrets %+v setup %q", res.Secrets, e.Setup)
	}
}
