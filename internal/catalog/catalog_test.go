package catalog

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
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
}
