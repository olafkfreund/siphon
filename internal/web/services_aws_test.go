package web

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const awsWebCfg = `server: { sandbox: none, db: DIR/s.db, public_url: "https://siphon.example", aws: {profiles: [p, sso-prod], role_arns: ['arn:aws:iam::123456789012:role/listed']}, mcp_packages: {
  aws-cloudwatch: {command: [cw], hosts: ['logs.{region}.amazonaws.com'], env: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION, AWS_DEFAULT_REGION, AWS_EC2_METADATA_DISABLED, AWS_CONFIG_FILE, AWS_SHARED_CREDENTIALS_FILE]},
  aws-docs: {command: [docs], hosts: [docs.aws.amazon.com]}} }
sources:
  gh: { type: webhook, secret: env:AGW_HOOK, signature: github }
`

func TestServicesAWS(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	// Role mode with base keys, both servers, webhook.
	w := ce.post("/services/aws", url.Values{"name": {"aws"}, "region": {"eu-west-1"}, "mode": {"role"},
		"role_arn": {"arn:aws:iam::123456789012:role/ro"}, "external_id": {"ext"}, "access_key_id": {"AKIAFAKEBASE"}, "secret_access_key": {"fakeBaseSecret"},
		"servers": {"cloudwatch", "docs"}, "webhook": {"on"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://siphon.example/hook/aws-hooks") || !strings.Contains(w.Body.String(), "X-Siphon-Key") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	cfg := ce.cur.Load()
	cr := cfg.Credentials["aws"]
	if cr == nil || cr.Provider != "aws" || cr.RoleARN != "arn:aws:iam::123456789012:role/ro" || cr.ExternalID != "ext" || cr.AccessKeyID.Value != "AKIAFAKEBASE" || cr.SecretAccessKey.Value != "fakeBaseSecret" {
		t.Fatalf("credential %+v", cr)
	}
	if fi, err := os.Stat(strings.TrimPrefix(cr.SecretAccessKey.Ref, "file:")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v", err)
	}
	if s := cfg.Sources["aws-cloudwatch"]; s == nil || s.Package != "aws-cloudwatch" || s.AWS != "aws" || s.Polled() {
		t.Fatalf("cloudwatch %+v", s)
	}
	if s := cfg.Sources["aws-docs"]; s == nil || s.Package != "aws-docs" || s.AWS != "" || s.Polled() || s.Read != nil || s.Poll != 0 {
		t.Fatalf("docs %+v", s)
	}
	h := cfg.Sources["aws-hooks"]
	if h == nil || h.Signature != "token" || h.TokenHeader != "X-Siphon-Key" || !strings.Contains(w.Body.String(), h.Secret.Value) {
		t.Fatalf("hook %+v", h)
	}
	for _, p := range []string{"/services", "/config/credentials/aws", "/history", "/history/export"} {
		b := ce.get(p).Body.String()
		if strings.Contains(b, "fakeBaseSecret") || strings.Contains(b, "AKIAFAKEBASE") || strings.Contains(b, h.Secret.Value) {
			t.Fatalf("secret visible on %s", p)
		}
	}
	if b := ce.get("/services").Body.String(); !strings.Contains(b, "aws-cloudwatch") || !strings.Contains(b, "AWS") {
		t.Errorf("rows missing: %s", b)
	}

	// Profile mode, CloudWatch only, no webhook.
	w = ce.post("/services/aws", url.Values{"name": {"prod"}, "region": {"us-east-1"}, "mode": {"profile"}, "profile": {"sso-prod"}, "servers": {"cloudwatch"}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	cfg = ce.cur.Load()
	if c := cfg.Credentials["prod"]; c == nil || c.Profile != "sso-prod" || c.Region != "us-east-1" || cfg.Sources["prod-hooks"] != nil || cfg.Sources["prod-docs"] != nil {
		t.Fatalf("profile mode: %+v", c)
	}

	// Refusals.
	bad := func(name string, v url.Values, want string) {
		t.Helper()
		v.Set("name", name)
		if v.Get("region") == "" {
			v.Set("region", "eu-west-1")
		}
		if w := ce.post("/services/aws", v); w.Code != 422 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d want %q", name, w.Code, want)
		}
	}
	bad("aws", url.Values{"mode": {"profile"}, "profile": {"p"}, "servers": {"docs"}}, "already exists")
	bad("x1", url.Values{"mode": {"role"}, "servers": {"cloudwatch"}}, "role ARN")
	bad("x2", url.Values{"mode": {"profile"}, "servers": {"cloudwatch"}}, "profile name")
	bad("x3", url.Values{"mode": {"profile"}, "profile": {"p"}}, "at least one server")
	bad("x4", url.Values{"mode": {"role"}, "role_arn": {"arn:aws:iam::123456789012:role/ro"}, "access_key_id": {"k"}, "servers": {"docs"}}, "go together")
	bad("x5", url.Values{"mode": {"role"}, "role_arn": {"not-an-arn"}, "servers": {"docs"}}, "role_arn")
	bad("X6", url.Values{"mode": {"profile"}, "profile": {"p"}, "servers": {"docs"}}, "lowercase")
}

func TestServicesAWSNeedsPackages(t *testing.T) {
	ce := newCfgEnv(t)
	if b := ce.get("/services").Body.String(); !strings.Contains(b, "enable <code>services.siphon.aws</code>") || strings.Contains(b, "/services/aws") {
		t.Errorf("tile should explain, not offer a form")
	}
	w := ce.post("/services/aws", url.Values{"name": {"aws"}, "region": {"eu-west-1"}, "mode": {"profile"}, "profile": {"p"}, "servers": {"cloudwatch"}})
	if w.Code != 422 || !strings.Contains(w.Body.String(), "services.siphon.aws") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestServicesAWSTest(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	ce.post("/services/aws", url.Values{"name": {"aws"}, "region": {"eu-west-1"}, "mode": {"profile"}, "profile": {"p"}, "servers": {"cloudwatch", "docs"}})
	var calls []string
	var fail error
	ce.testAWS = func(_ context.Context, src string) (string, time.Time, []string, error) {
		calls = append(calls, src)
		return "arn:aws:sts::123456789012:assumed-role/ro/siphon-test", time.Date(2030, 1, 1, 12, 30, 0, 0, time.UTC), []string{"a", "b", "c"}, fail
	}
	got := ce.post("/services/aws-cloudwatch/test", nil).Body.String()
	if !strings.Contains(got, "assumed-role/ro/siphon-test") || !strings.Contains(got, "12:30 UTC") || !strings.Contains(got, "3 tools") || len(calls) != 1 || calls[0] != "aws-cloudwatch" {
		t.Fatalf("%v %s", calls, got)
	}
	fail = errors.New("the bridge exited before it listened")
	got = ce.post("/services/aws-cloudwatch/test", nil).Body.String()
	if !strings.Contains(got, "exited before it listened") || !strings.Contains(got, "assumed-role/ro") || strings.Contains(got, "tools") {
		t.Fatalf("%s", got)
	}
	if got := ce.post("/services/aws-docs/test", nil).Body.String(); !strings.Contains(got, "nothing to test") || len(calls) != 2 {
		t.Fatalf("docs: %s", got)
	}
}

func TestAWSCredentialNotOnConnectionsPage(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	ce.post("/services/aws", url.Values{"name": {"awsprod"}, "region": {"eu-west-1"}, "mode": {"profile"}, "profile": {"p"}, "servers": {"cloudwatch"}})
	if ce.cur.Load().Credentials["awsprod"] == nil {
		t.Fatal("not created")
	}
	if b := ce.get("/connections").Body.String(); strings.Contains(b, "awsprod") {
		t.Errorf("aws credential listed as a subscription")
	}
}

func TestServicesAWSAllowlist(t *testing.T) {
	ce := newCfgEnvFile(t, awsWebCfg)
	page := ce.get("/services").Body.String()
	if !strings.Contains(page, "<option>sso-prod</option>") || !strings.Contains(page, `<option value="arn:aws:iam::123456789012:role/listed">`) || strings.Contains(page, `value="profile" disabled`) {
		t.Errorf("tile does not offer the lists")
	}
	form := func(extra url.Values) url.Values {
		v := url.Values{"name": {"al"}, "region": {"eu-west-1"}, "servers": {"cloudwatch"}}
		for k, x := range extra {
			v[k] = x
		}
		return v
	}
	for name, tc := range map[string]struct {
		v    url.Values
		want string
	}{
		"profile off list": {url.Values{"mode": {"profile"}, "profile": {"admin"}}, "not in server.aws.profiles"},
		"role off list":    {url.Values{"mode": {"role"}, "role_arn": {"arn:aws:iam::999999999999:role/evil"}}, "not in server.aws.role_arns"},
	} {
		if w := ce.post("/services/aws", form(tc.v)); w.Code != 422 || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	// Own keys free the role from the list.
	w := ce.post("/services/aws", form(url.Values{"mode": {"role"}, "role_arn": {"arn:aws:iam::999999999999:role/evil"}, "access_key_id": {"AKIAFAKE"}, "secret_access_key": {"fakeSecret"}}))
	if w.Code != 200 {
		t.Errorf("own keys: %d %s", w.Code, w.Body.String())
	}
	// On the list, no keys.
	if w := ce.post("/services/aws", form(url.Values{"name": {"al2"}, "mode": {"role"}, "role_arn": {"arn:aws:iam::123456789012:role/listed"}})); w.Code != 200 {
		t.Errorf("listed role: %d", w.Code)
	}
}

func TestServicesAWSEmptyProfileList(t *testing.T) {
	ce := newCfgEnvFile(t, strings.Replace(awsWebCfg, "profiles: [p, sso-prod]", "profiles: []", 1))
	page := ce.get("/services").Body.String()
	if !strings.Contains(page, `value="profile" disabled`) || !strings.Contains(page, "server.aws.profiles</code> in siphon.yaml") {
		t.Errorf("profile mode should be disabled with a hint")
	}
}
