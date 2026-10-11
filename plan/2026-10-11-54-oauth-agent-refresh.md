---
status: draft
issue: 54
spec: spec/2026-10-11-54-oauth-agent-refresh.md
---

# Plan: a daemon-side proxy for OAuth MCP sources in agent runs

Self-contained. The approved decisions:

- **The proxy:** an agent run reaches each `auth.oauth` MCP source
  through a per-run proxy that the daemon serves.
  - The agent's MCP config gets `http://127.0.0.1:<port>/mcp` and
    `Authorization: Bearer <per-run key>` (`randomHex(32)`, masked).
  - It never gets the access token, the refresh token or the source's
    static `headers:`.
- **Listener:**
  - **systemd mode:** a unix socket in a fresh `newRunDir`. It's 0660
    with the run dir's group, created on a `.tmp` name and renamed into
    place (as `egress.Proxy.ServeUnix` does), and forwarded through the
    existing `forwards[port] = sock`.
  - **`none` mode:** `127.0.0.1:0` in the daemon.
- **The handler:**
  - **Key:** compared in constant time. Otherwise 401, without calling
    upstream.
  - **Path:** only `/mcp`; anything else gets 404. Every request goes to
    the source's exact `url`, plus the agent's query string.
  - **Headers:**
    - the agent's `Authorization`, `Cookie` and `Proxy-*` headers are
      removed;
    - the source's static headers are set;
    - `Authorization: Bearer <Token(ctx)>` is set, fetched on every
      request (`mcpoauth.Manager.Token`, which refreshes within `skew`);
    - the rest passes through.
  - **A token error** gives 401 with the masked error text, without
    calling upstream.
  - **Forwarding:** `httputil.ReverseProxy` with `Rewrite`,
    `FlushInterval: -1`, and the source's guarded stream client (no
    overall timeout, no body cap, no redirects, the private-address rules
    with `allow_private`).
  - **An upstream error** gives 502 with a masked, clipped message.
    Nothing is logged with a token.
- **Lifetime:** the proxy stops with the run, like the bridges; the
  listener is closed and the run dir removed.
- **Pipeline:**
  - check the login once before the run (fail "OAuth login required" as
    today), then hand the proxy a token func;
  - the access token is no longer put in the agent's headers or
    `secrets`.
- **Egress:** `AgentEgress` no longer adds an OAuth source's host.
- **No `allowed_tools` enforcement in the proxy** (resolved at the
  intent). No new dependency, config, schema or migration.

## Steps

1. **The stream client and egress** (coder).
   - **`internal/source/netguard.go:28-30`:** add
     `func StreamClient(allowPrivate bool) *http.Client { return guardedClient(allowPrivate, 0, 0, true) }`,
     with a comment: no overall timeout, no body cap, no redirects.
   - **`internal/config/config.go`, `AgentEgress` (`:1772-1792`):** skip
     the URL host when `src.Auth != nil && src.Auth.OAuth != nil`, with a
     comment: the agent reaches it through the daemon's proxy (#54).
   - **Tests:**
     - in `internal/config/config_test.go`, next to
       `TestAgentEgressFailsClosed` (`:588`): an agent with an OAuth
       source and a bearer source. The bearer host is in `allow`, the
       OAuth host isn't.
     - in `internal/source`, if it's cheap: `StreamClient` doesn't
       follow redirects.
   - **Verify:** `go test ./internal/config ./internal/source`.

2. **The proxy** (coder).
   - **`internal/action/agent.go:20-30`:** `MCPServer` gains
     `Token func(context.Context) (string, error)`, with a comment that
     it's a remote OAuth server reached through the daemon's proxy, and
     that it isn't serialised. Also `AllowPrivate bool`, for the proxy's
     dial.
   - **New `internal/action/oauthproxy.go`:**
     - `oauthProxy(s MCPServer, key string, mask []string) http.Handler`,
       as described in the decisions;
     - `startOAuthProxy(ctx, o *AgentOptions, s MCPServer, key string) (sock string, port int, stop func(), err error)`:
       - **systemd mode:** `newRunDir(o.Sandbox.Dir)`, then the socket
         `mcp.sock` (with `.tmp`, chmod 0660, chown to the dir's group,
         ignoring `EPERM` like `ServeUnix`, then rename), and `http.Serve`
         in a goroutine;
       - **`none` mode:** listen on `127.0.0.1:0` and return its port;
       - **`stop`:** `srv.Close()` and `os.RemoveAll(runDir)`.
   - **`internal/action/bridge.go`, `startBridges` (`:52-100`):**
     - in the loop, before the stdio check, handle `s.Token != nil` with
       `startOAuthProxy`;
     - **systemd mode:** `forwards[port] = sock`, with `port = bridgeBasePort+i`;
     - **`none` mode:** the listener's own port, with no forward;
     - `stops`, `direct` and
       `mcp[name] = MCPServer{URL: "http://127.0.0.1:<port>/mcp", Headers: {"Authorization": "Bearer " + key}}`,
       exactly as for a bridge;
     - add the key to `o.Secrets`;
     - skip the "needs a restricted agent" check: it's for stdio
       secrets, not this.
   - **Tests** (new `internal/action/oauthproxy_test.go`):
     - **The handler,** with an `httptest` upstream that records
       headers:
       - no key or a wrong key gets 401, with zero upstream calls;
       - `/other` gets 404;
       - with the right key, the upstream sees `Bearer t1` and the static
         header, but not the agent's `Authorization` or `Cookie`;
       - when the token func returns `t2` next, the next request carries
         `t2`;
       - a token func error gives 401, the body holds the message, and
         there are zero upstream calls;
       - **SSE:** the upstream writes `data: 1\n\n`, flushes and blocks
         until the test releases it. The client reads `data: 1` first,
         with a timeout;
       - a query string is forwarded.
     - **`startOAuthProxy` in `none` mode:** a request through
       `127.0.0.1:<port>` works, and after `stop` the port refuses.
     - **`startBridges` with a `Token` server in `none` mode:** `o.MCP`
       holds a loopback URL and a `Bearer` key, with no `Token` and no
       upstream URL, and the key is in `o.Secrets`.
   - **Verify:** `go vet ./... && go test -race ./internal/action`.
   - **Traps:**
     - **Unix socket paths are ≤ 107 bytes:** tests that create sockets
       use a short `/tmp` dir, not `t.TempDir()`;
     - never log or return the token, or the key unmasked;
     - no real-clock sleeps: use channels for the SSE test.

3. **The pipeline** (coder).
   - **`internal/job/pipeline.go`, `agentExec` (`:452-462`):** for an
     OAuth source:
     - call `p.OAuth.Token(ctx, name)` once and fail as today on an
       error, discarding the token;
     - then
       `servers[name] = action.MCPServer{URL: s.URL, Headers: h, AllowPrivate: s.AllowPrivate, Token: func(c context.Context) (string, error) { return p.OAuth.Token(c, name) }}`;
     - don't set `h["Authorization"]`, and don't append the token to
       `secrets`.
   - **`internal/job/oauth_test.go`, `TestAgentRunOAuthToken`:** rename it
     to `TestAgentRunOAuthProxy`. Keep the no-login case. With the login:
     - the output (the dumped MCP config) has no `ACCESS-TOK` and no
       `REFRESH-SECRET`;
     - it has a `http://127.0.0.1:` URL for `m1`;
     - it has `Bearer ***`, the masked key.
   - **Also fix any other test** that expected the token in the config.
   - **Verify:** `go vet ./... && go test -race ./internal/job ./internal/action ./internal/config`.

4. **The VM test** (Opus).
   - **New `nix/stub-mcp-http.py`:** a minimal Streamable HTTP MCP
     server with plain JSON responses on `POST /mcp`. It answers:
     - `initialize`;
     - `notifications/initialized` (202);
     - `tools/list`, with one tool, `whoami`;
     - `tools/call whoami`, which returns
       `auth-sha=<sha256 of the Authorization header value>`.
     - It listens on `127.0.0.1:9101`, as a systemd service like
       `stub-model`.
   - **`nix/vm-test.nix`:**
     - a source `oauthhttp`: `type = "mcp"`,
       `url = "http://127.0.0.1:9101/mcp"`, `allow_private = true`,
       `auth.oauth = { }`;
     - an agent `oauthagent` like `bridged` (`kind = "model"`, the stub
       model), with `mcp = [ "oauthhttp" ]` and
       `allowed_tools = [ "mcp__oauthhttp__whoami" ]`;
     - a rule on the webhook source for `kind == "oauth"`.
   - **The subtest:**
     - write a login to
       `/var/lib/siphon/credentials/.mcp/oauthhttp/mcp-oauth.json`
       (owner siphon, 0600, dir 0700): `resource` the source URL,
       `dynamic: true`, `client_id`, `token_url`, and an access token
       `VM-ACCESS-TOK` that expires in 2100;
     - fire the hook and wait for `done`;
     - check the output contains `auth-sha=<sha256("Bearer VM-ACCESS-TOK")>`,
       and doesn't contain `VM-ACCESS-TOK`.
     - This proves systemd mode end to end: the unit, the socket and the
       forward.
   - **Check:** how the stub model drives a tool call (`nix/stub-model.py`)
     and how the `bridged` subtest (`:703`) reads its result. Copy that.
   - **Verify:** `nix build .#checks.x86_64-linux.vm-test -L`, or CI.
   - **Traps:**
     - the test script is type-checked;
     - write any `systemctl` text with the Write tool;
     - the flake sees only git-tracked files, so `git add` the stub
       first.

5. **Docs** (Opus).
   - **`docs/tasks/connect-oauth-mcp.md`, "Afterwards":** agent runs
     reach the server through Siphon, which adds a current token to every
     request. Long runs keep working, and the agent never sees a token or
     the source's headers. The agent's egress allowlist no longer needs
     the MCP host.
   - **Also:** `go generate ./docs`.
   - **Verify:** `go test ./docs/`.

6. **Review.**
   - A fresh Opus security review gets this plan and `git diff main...`.
   - Fix the findings, and log the deviations here.

## Tests

- `go vet ./... && go test -race ./...`: all pass.
- CI's `nix flake check` passes, including the new VM subtest.

## Rollback

- **Revert the merge.** There's no config, data or schema change.
- **Agents that need the MCP host in egress for other traffic** keep any
  explicit `egress.allow` entry, unaffected either way.
