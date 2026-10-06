package job

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/rule"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

// TestHelperAgent is not a test: it is the stand-in agent CLI, run as a
// child process. It fetches two URLs through $HTTPS_PROXY (Go honours it)
// and prints what happened.
func TestHelperAgent(t *testing.T) {
	if os.Getenv("AGW_HELPER") != "1" {
		return
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	for _, k := range []string{"AGW_OK", "AGW_BAD"} {
		resp, err := c.Get(os.Getenv(k))
		if err != nil {
			fmt.Printf("%s=error\n", k)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s=%d %s\n", k, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	os.Exit(0)
}

// Plan #12 step 4: an agent run gets the proxy; allowed hosts work, others
// are refused, and refusals land in the job output and the audit log.
func TestAgentEgressThroughProxy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "reached")
	}))
	defer upstream.Close()
	exe, _ := os.Executable()
	dir := t.TempDir()
	stub := filepath.Join(dir, "agent")
	script := fmt.Sprintf("#!/bin/sh\nexport AGW_HELPER=1 AGW_OK=%s/ AGW_BAD=https://blocked.invalid/\nexec %s -test.run=TestHelperAgent\n", upstream.URL, exe)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The allowed upstream is on loopback, so it enters the allowlist the way
	// a LAN MCP server would: an MCP source with allow_private.
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db }
sources:
  s:  { type: http, url: https://example.com/x }
  up: { type: mcp, url: ` + upstream.URL + `/mcp, read: { resource: "x://a" }, allow_private: true }
agents:
  probe: { command: ` + stub + `, prompt: "p", mcp: [up], approve: false }
rules:
  - { name: go, source: s, when: 'true', cooldown: 1s, action: { agent: probe } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Server.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := New(cfg, st, time.Now)
	ctx := context.Background()
	if _, _, err := p.HandleEvent(ctx, rule.Event{Source: "s", Data: map[string]any{}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RunQueued(ctx); err != nil {
		t.Fatal(err)
	}
	var out string
	st.DB.QueryRow(`SELECT output FROM jobs WHERE rule='go'`).Scan(&out)
	if !strings.Contains(out, "AGW_OK=200 reached") {
		t.Fatalf("allowed host not reached through the proxy:\n%s", out)
	}
	if strings.Contains(out, "AGW_BAD=200") {
		t.Fatalf("blocked host was reachable:\n%s", out)
	}
	if !strings.Contains(out, "egress: blocked blocked.invalid:443 (1)") {
		t.Fatalf("blocked host not reported:\n%s", out)
	}
	if count(t, p, `SELECT count(*) FROM audit WHERE event='egress_blocked' AND detail='blocked.invalid:443'`) != 1 {
		t.Fatal("egress_blocked not audited")
	}
	if strings.Contains(out, "run-") && strings.Contains(out, "@") {
		t.Fatalf("proxy credential leaked into output:\n%s", out)
	}
}
