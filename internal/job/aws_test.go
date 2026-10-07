package job

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

const awsJobCfg = `
server: { sandbox: none, db: DIR/state.db, mcp_packages: {cw: {command: [srv], hosts: ['logs.{region}.amazonaws.com'],
  env: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION, AWS_DEFAULT_REGION, AWS_EC2_METADATA_DISABLED, AWS_CONFIG_FILE, AWS_SHARED_CREDENTIALS_FILE]}} }
credentials:
  aws1: { provider: aws, region: eu-west-1, role_arn: 'arn:aws:iam::123456789012:role/r', access_key_id: 'env:AGW_AK', secret_access_key: 'env:AGW_SK' }
sources:
  cw: { type: mcp, package: cw, aws: aws1 }
agents:
  ops: { kind: codex, command: /bin/true, mcp: [cw], prompt: x }
`

func awsPipeline(t *testing.T, sts int) *Pipeline {
	t.Setenv("AGW_AK", "AKIABASEFAKE")
	t.Setenv("AGW_SK", "baseSecretFake")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sts != 200 {
			w.WriteHeader(sts)
			w.Write([]byte(`<ErrorResponse><Error><Code>AccessDenied</Code><Message>denied for baseSecretFake</Message></Error></ErrorResponse>`))
			return
		}
		w.Write([]byte(`<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIATEMPFAKE</AccessKeyId><SecretAccessKey>tempSecretFake</SecretAccessKey><SessionToken>tempTokenFake</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL_STS", srv.URL)
	return applyPipeline(t, awsJobCfg)
}

func runOps(p *Pipeline) (string, string) {
	state, _, out, _ := p.agentExec(context.Background(), p.Config(), store.QueuedJob{ID: 5}, Payload{Action: config.Action{Agent: "ops"}}, true)
	return state, out
}

// The bridge gets temporary keys and the region; its error masks every value
// and never shows a base key.
func TestAWSBridgeEnv(t *testing.T) {
	dump := bridgeEnvDump(os.Getpid())
	os.Remove(dump)
	t.Cleanup(func() { os.Remove(dump) })
	state, out := runOps(awsPipeline(t, 200))
	if state != "failed" || !strings.Contains(out, `mcp bridge for "cw"`) {
		t.Fatalf("%s %q", state, out)
	}
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"AWS_ACCESS_KEY_ID": "ASIATEMPFAKE", "AWS_SECRET_ACCESS_KEY": "tempSecretFake", "AWS_SESSION_TOKEN": "tempTokenFake",
		"AWS_REGION": "eu-west-1", "AWS_DEFAULT_REGION": "eu-west-1", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null"}
	if !maps.Equal(env, want) {
		t.Errorf("bridge env %v", env)
	}
	for _, leak := range []string{"ASIATEMPFAKE", "tempSecretFake", "tempTokenFake", "AKIABASEFAKE", "baseSecretFake"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %s", leak)
		}
	}
}

func TestAWSFailureStopsBeforeAgent(t *testing.T) {
	state, out := runOps(awsPipeline(t, 403))
	if state != "failed" || !strings.HasPrefix(out, "aws credentials for cw: ") || strings.Contains(out, "baseSecretFake") {
		t.Fatalf("%s %q", state, out)
	}
	if strings.Contains(out, "mcp bridge") {
		t.Errorf("bridge started: %q", out)
	}
}

func TestAWSSourceIsNotPolled(t *testing.T) {
	p := awsPipeline(t, 200)
	ctx, cancel := context.WithCancel(context.Background())
	stop := p.startPollers(ctx, p.Config())
	time.Sleep(100 * time.Millisecond)
	cancel()
	stop()
	st, err := store.SourceStates(p.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st["cw"]; ok {
		t.Errorf("aws source was polled: %+v", st["cw"])
	}
}
