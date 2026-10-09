package job

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

// An agent run gets the OAuth access token as an Authorization header, masked
// in the job output, never the refresh token; with no login the job fails
// with the login message.
func TestAgentRunOAuthToken(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "cat-config.sh")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --mcp-config ] && cat \"$2\"; shift; done\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	const url = "https://m1.example/mcp"
	cfg, err := config.Parse([]byte(`
server: { sandbox: none, db: ` + dir + `/state.db, public_url: "https://gw.example" }
sources:
  m1: { type: mcp, url: ` + url + `, read: { resource: "x://a" }, auth: { oauth: {} } }
  m2: { type: mcp, url: https://m2.example/mcp, read: { resource: "x://b" } }
agents:
  a: { runner: [` + stub + `], prompt: "p", mcp: [m1], approve: false }
rules:
  - { name: r, source: m2, when: 'true', on: each, id: "string(event.id)", cooldown: 1s, action: { agent: a } }
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
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	p := New(cfg, st, func() time.Time { return time.Unix(0, clock.Load()) })
	ctx := context.Background()
	n := 0
	run := func() (status, out string) {
		t.Helper()
		n++
		if _, _, err := p.HandleEvent(ctx, rule.Event{Source: "m2", Data: map[string]any{"id": n}}, false); err != nil {
			t.Fatal(err)
		}
		if _, err := p.RunQueued(ctx); err != nil {
			t.Fatal(err)
		}
		if err := st.DB.QueryRow(`SELECT state, output FROM jobs WHERE rule='r' ORDER BY id DESC LIMIT 1`).Scan(&status, &out); err != nil {
			t.Fatal(err)
		}
		return
	}
	if status, out := run(); status != "failed" && status != "queued" || !strings.Contains(out, "siphon connect oauth m1") {
		t.Fatalf("no login: %s %q", status, out)
	}

	login, _ := json.Marshal(map[string]any{"resource": url, "dynamic": true, "client_id": "c", "auth_url": "https://as/a", "token_url": "https://as/t",
		"token": map[string]any{"access_token": "ACCESS-TOK", "refresh_token": "REFRESH-SECRET", "token_type": "Bearer", "expiry": time.Now().Add(time.Hour)}})
	d := filepath.Join(dir, "credentials", ".mcp", "m1")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "mcp-oauth.json"), login, 0o600); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(2 * time.Second)) // past the rule cooldown
	status, out := run()
	if strings.Contains(out, "ACCESS-TOK") || strings.Contains(out, "REFRESH-SECRET") || !strings.Contains(out, "Bearer ***") {
		t.Fatalf("token missing or leaked: %s %s", status, out)
	}
}
