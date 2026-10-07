package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const awsPkgs = `server: {mcp_packages: {
  cw: {command: [cw], hosts: ['logs.{region}.amazonaws.com', 'monitoring.{region}.amazonaws.com'], env: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION, AWS_DEFAULT_REGION, AWS_EC2_METADATA_DISABLED, AWS_CONFIG_FILE, AWS_SHARED_CREDENTIALS_FILE]},
  short: {command: [s], hosts: [a.example], env: [AWS_REGION]},
  docs: {command: [d], hosts: ['docs.aws.amazon.com']}}}
`

const awsRole = "{provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/siphon', external_id: x}"

func TestAWSValidation(t *testing.T) {
	t.Setenv("AGW_K", "k")
	src := "{type: mcp, package: cw, aws: a}"
	for name, y := range map[string]string{
		"ok role":               "credentials: {a: " + awsRole + "}\nsources: {s: " + src + "}",
		"ok profile":            "credentials: {a: {provider: aws, region: us-east-1, profile: p}}\nsources: {s: " + src + "}",
		"ok base keys":          "credentials: {a: {provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/a/b', access_key_id: 'env:AGW_K', secret_access_key: 'env:AGW_K'}}\nsources: {s: " + src + "}",
		"bad region":            "credentials: {a: {provider: aws, region: Europe, profile: p}}",
		"both":                  "credentials: {a: {provider: aws, region: eu-west-1, profile: p, role_arn: 'arn:aws:iam::123456789012:role/a'}}",
		"neither":               "credentials: {a: {provider: aws, region: eu-west-1}}",
		"bad arn":               "credentials: {a: {provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::12:role/a'}}",
		"ext w/o role":          "credentials: {a: {provider: aws, region: eu-west-1, profile: p, external_id: x}}",
		"half pair":             "credentials: {a: {provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/a', access_key_id: 'env:AGW_K'}}",
		"keys w/ profile":       "credentials: {a: {provider: aws, region: eu-west-1, profile: p, access_key_id: 'env:AGW_K', secret_access_key: 'env:AGW_K'}}",
		"aws fields elsewhere":  "credentials: {a: {provider: claude, region: eu-west-1}}",
		"agent names aws":       "credentials: {a: " + awsRole + "}\nagents: {g: {kind: claude, credential: a}}",
		"source no cred":        "sources: {s: " + src + "}",
		"source wrong provider": "credentials: {c: {provider: claude}}\nsources: {s: {type: mcp, package: cw, aws: c}}",
		"source no package":     "credentials: {a: " + awsRole + "}\nsources: {s: {type: mcp, command: [x], aws: a}}",
		"package env short":     "credentials: {a: " + awsRole + "}\nsources: {s: {type: mcp, package: short, aws: a}}",
		"poll set":              "credentials: {a: " + awsRole + "}\nsources: {s: {type: mcp, package: cw, aws: a, poll: 1m}}",
		"read set":              "credentials: {a: " + awsRole + "}\nsources: {s: {type: mcp, package: cw, aws: a, read: {tool: t}}}",
		"region w/o aws":        "sources: {s: {type: mcp, package: cw, read: {tool: t}}}",
		"timeout 56m":           "credentials: {a: " + awsRole + "}\nsources: {s: " + src + "}\nagents: {g: {kind: claude, credential: c, mcp: [s], timeout: 56m}}\n",
		"timeout 55m":           "credentials: {a: " + awsRole + ", c: {provider: claude}}\nsources: {s: " + src + "}\nagents: {g: {kind: claude, credential: c, mcp: [s], timeout: 55m}}\n",
		"docs no aws":           "sources: {s: {type: mcp, package: docs, read: {tool: t}}}",
	} {
		c, err := Parse([]byte(awsPkgs + y))
		if err == nil {
			err = c.Validate()
		}
		want := !strings.HasPrefix(name, "ok") && name != "timeout 55m" && name != "docs no aws"
		if want == (err == nil) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The 56m case fails on the timeout rule and nothing else.
	c, _ := Parse([]byte(awsPkgs + "credentials: {a: " + awsRole + ", c: {provider: claude}}\nsources: {s: " + src + "}\nagents: {g: {kind: claude, credential: c, mcp: [s], timeout: 56m}}\n"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "55m or less") {
		t.Errorf("timeout: %v", err)
	}
}

func TestAWSHostsAndPolling(t *testing.T) {
	c, err := Parse([]byte(awsPkgs + "credentials: {a: " + awsRole + ", c: {provider: claude}}\nsources: {s: {type: mcp, package: cw, aws: a}}\nagents: {g: {kind: claude, credential: c, mcp: [s]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hostsOf(c.BridgeEgress(c.Sources["s"])); got != hostsOf([]HostPort{{Host: "logs.eu-west-1.amazonaws.com", Port: 443}, {Host: "monitoring.eu-west-1.amazonaws.com", Port: 443}}) {
		t.Errorf("bridge hosts: %s", got)
	}
	if hosts, _ := c.AgentEgress(c.Agents["g"]); strings.Contains(hostsOf(hosts), "amazonaws") {
		t.Errorf("agent egress has bridge hosts: %s", hostsOf(hosts))
	}
	if c.Sources["s"].Poll != 0 || c.Sources["s"].Polled() {
		t.Errorf("aws source is polled: %v", c.Sources["s"].Poll)
	}
}

func TestAWSSecretsResolved(t *testing.T) {
	t.Setenv("AGW_AK", "AKIAFAKE")
	t.Setenv("AGW_SK", "fake-secret")
	c, err := Parse([]byte("credentials: {a: {provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/a', access_key_id: 'env:AGW_AK', secret_access_key: 'env:AGW_SK'}}"))
	if err != nil {
		t.Fatal(err)
	}
	if cr := c.Credentials["a"]; cr.AccessKeyID.Value != "AKIAFAKE" || cr.SecretAccessKey.Value != "fake-secret" {
		t.Errorf("not resolved: %+v", cr)
	}
	if s := c.Secrets(); !strings.Contains(strings.Join(s, " "), "fake-secret") {
		t.Errorf("not masked: %v", s)
	}
	c, _ = Parse([]byte("credentials: {a: {provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/a', access_key_id: literal, secret_access_key: literal}}"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "inline secret") {
		t.Errorf("inline secret accepted: %v", err)
	}
}

func TestOverlayAWS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte(awsPkgs[:len(awsPkgs)-2]+", db: "+dir+"/s.db}\n"+
		"credentials:\n  base: {provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/a', access_key_id: 'env:HOME', secret_access_key: 'env:HOME'}\n"), 0o600)
	// Round trip: the portal adds a credential and a source.
	eff, _, err := LoadWithOverlay(path, []Item{
		{Kind: "credentials", Name: "p", YAML: "{provider: aws, region: us-east-1, profile: sso}"},
		{Kind: "sources", Name: "cw", YAML: "{type: mcp, package: cw, aws: p}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Sources["cw"].AWS != "p" || eff.Credentials["p"].Profile != "sso" || eff.Credentials["p"].Region != "us-east-1" {
		t.Errorf("round trip: %+v %+v", eff.Sources["cw"], eff.Credentials["p"])
	}
	// The file's base keys may not follow a changed role or region; foreign refs are refused.
	for name, y := range map[string]string{
		"new role":   "{provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::999999999999:role/evil', access_key_id: 'env:HOME', secret_access_key: 'env:HOME'}",
		"new region": "{provider: aws, region: us-east-1, role_arn: 'arn:aws:iam::123456789012:role/a', access_key_id: 'env:HOME', secret_access_key: 'env:HOME'}",
		"foreign":    "{provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/a', access_key_id: 'file:/etc/passwd', secret_access_key: 'env:HOME'}",
	} {
		if _, _, err := LoadWithOverlay(path, []Item{{Kind: "credentials", Name: map[bool]string{true: "base", false: "n"}[name != "foreign"], YAML: y}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := LoadWithOverlay(path, []Item{{Kind: "credentials", Name: "base", YAML: "{provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/a', access_key_id: 'env:HOME', secret_access_key: 'env:HOME', external_id: y}}"}}); err == nil {
		t.Error("external_id change keeps file keys")
	}
}

func TestToolsOnlyMCPSource(t *testing.T) {
	parse := func(y string) (*Config, error) {
		c, err := Parse([]byte(awsPkgs + y))
		if err == nil {
			err = c.Validate()
		}
		return c, err
	}
	c, err := parse("sources: {d: {type: mcp, package: docs}}\ncredentials: {c: {provider: claude}}\nagents: {g: {kind: claude, credential: c, mcp: [d]}}")
	if err != nil || c.Sources["d"].Polled() || c.Sources["d"].Poll != 0 || hasWarning(c, "does nothing") {
		t.Fatalf("tools-only: %v polled=%v warnings=%v", err, c.Sources["d"].Polled(), c.Warnings())
	}
	if c, _ := parse("sources: {d: {type: mcp, package: docs}}"); !hasWarning(c, "source d: no read and no agent uses it") {
		t.Errorf("no warning: %v", c.Warnings())
	}
	if _, err := parse("sources: {d: {type: mcp, package: docs, poll: 1m}}"); err == nil || !strings.Contains(err.Error(), "poll needs read") {
		t.Errorf("poll without read: %v", err)
	}
	// With read nothing changes: polled, default poll, read still checked.
	c, err = parse("sources: {d: {type: mcp, package: docs, read: {tool: t}}}")
	if err != nil || !c.Sources["d"].Polled() || c.Sources["d"].Poll == 0 {
		t.Errorf("with read: %v", err)
	}
	if _, err := parse("sources: {d: {type: mcp, package: docs, read: {}}}"); err == nil || !strings.Contains(err.Error(), "read needs exactly one") {
		t.Errorf("empty read: %v", err)
	}
}
