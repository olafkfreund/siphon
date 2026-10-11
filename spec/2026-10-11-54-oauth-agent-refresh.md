---
status: draft
issue: 54
intent: intent/2026-10-11-54-oauth-agent-refresh.md
---

# Spec: a daemon-side proxy for OAuth MCP sources in agent runs

## Design

The agent reaches each `auth.oauth` source through a per-run proxy that
the daemon serves. This is the same shape bridged stdio servers already
use (`internal/action/bridge.go`, `startBridges`):
- the agent's MCP config gets `http://127.0.0.1:<port>/mcp` and a
  per-run key;
- the daemon forwards to the source's own `url`, with a token fetched
  per request.

### `internal/action`

- **`MCPServer`** (`agent.go:~20`) gains
  `Token func(context.Context) (string, error)` (not serialised). When
  it's set, the server is a remote OAuth server to be proxied.
- **`startBridges`** (`bridge.go:40-103`) handles a server with
  `Token != nil` next to the stdio bridges, in a new file,
  `oauthproxy.go`:
  1. **A per-run key:** `randomHex(32)`, added to `o.Secrets` so it's
     masked.
  2. **A listener:**
     - **systemd mode:** a unix socket `mcp.sock` in a fresh
       `newRunDir(o.Sandbox.Dir)`. It's mode 0660, with the run dir's
       group, created on a temporary name and renamed into place, like
       `egress.Proxy.ServeUnix`. `forwards[port] = sock`, so the agent
       unit's exec-job forwards `127.0.0.1:<port>` to it. That's the
       existing mechanism, and the unit gets no new network access.
     - **`none` mode:** a TCP listener on `127.0.0.1:0` in the daemon.
       The per-run key is what keeps other local processes out.
  3. **The rewrite:** `mcp[name]` becomes
     `MCPServer{URL: "http://127.0.0.1:<port>/mcp", Headers: {"Authorization": "Bearer <key>"}}`,
     and the port is added to `direct` (no egress proxy for loopback).
  4. **Stop:** closing the listener ends the server, and the run dir is
     removed. It's added to `stops`, so it ends with the run, as the
     bridges do.
- **The proxy handler** (`oauthProxy`, an `http.Handler`):
  - **The key:** `Authorization` must equal `Bearer <key>`, compared in
    constant time. Otherwise 401.
  - **The path:** only `/mcp` is served; anything else gets 404. The
    agent can't choose the upstream host or path: every request goes to
    the source's exact `url`, with the agent's query string, as MCP
    Streamable HTTP uses one endpoint.
  - **Request headers:**
    - the agent's `Authorization`, `Cookie` and `Proxy-*` headers are
      removed;
    - the source's static `headers:` are set (they no longer go to the
      agent);
    - `Authorization: Bearer <Token(ctx)>` is set.
    - The rest (`Content-Type`, `Accept`, `Mcp-Session-Id`,
      `MCP-Protocol-Version`, `Last-Event-ID`) passes through.
  - **The token is fetched on every request** through
    `mcpoauth.Manager.Token`. That returns the stored token and refreshes
    it within `skew` of expiry, under the existing per-source lock, so
    there's no network call per request.
    - If it fails (login required, refresh revoked), the response is
      **401** with the masked error text, so the agent's output says
      "OAuth login required: …".
  - **Forwarding** uses `httputil.ReverseProxy`:
    - `Rewrite` sets the URL to the source's `url`;
    - `FlushInterval: -1`, so server-sent events stream unbuffered;
    - the transport comes from the source's guarded client in stream mode
      (private-address rules, `allow_private`, no redirects), with no
      overall timeout and no body cap. A new exported
      `source.StreamClient(allowPrivate bool) *http.Client` wraps
      `guardedClient(…, true)`.
    - The request context is cancelled when the agent disconnects or the
      run ends.
  - **Upstream errors:** `ErrorHandler` writes 502 with a masked,
    clipped message. Nothing is logged with a token.
  - **Responses:** upstream 3xx responses pass through unchanged, since
    the client doesn't follow redirects. They aren't rewritten.

### `internal/job/pipeline.go` (`agentExec`, `:452-462`)

- **For an `auth.oauth` source:** instead of fetching a token into a
  static header, set
  `Token: func(ctx) { return p.OAuth.Token(ctx, name) }` and keep `h`
  (the static headers) for the proxy.
- **Check the login before the run starts:** call `p.OAuth.Token` once
  and fail as today ("OAuth login required") if there's none. The
  returned token isn't used or kept.
- **Masking:** the access token is no longer added to `secrets`, since
  it never reaches the run. The key is masked instead.
- **`AllowPrivate`** reaches the proxy through the server definition, so
  the source's own setting governs the daemon's dial.

### `internal/config` (`AgentEgress`, `config.go:1772-1792`)

- An `auth.oauth` source's host is no longer added to the agent's egress
  allowlist. The agent reaches it only through the loopback proxy.

### Docs

- **`docs/tasks/connect-oauth-mcp.md`, "Afterwards":**
  - "Agent runs get a current access token, never the refresh token"
    becomes: agent runs reach the server through Siphon. Siphon adds a
    current token to every request, so long runs keep working, and the
    agent never sees a token.
  - The agent's egress allowlist no longer needs the MCP host.

## Alternatives rejected

- **Refresh before the run** (option b): rejected at the intent.
- **Run the proxy as its own `siphon-mcp@` unit,** like stdio bridges.
  The unit would need the access and refresh tokens, or a channel back
  to the daemon. The daemon already holds the tokens, the lock and the
  guarded client, so serving the socket there adds no new trust.
- **Hand the agent short-lived tokens through a file it re-reads.**
  Agent CLIs read MCP headers once, at start.
- **Enforce `allowed_tools` in the proxy:** rejected at the intent.

## Risks

- **Streaming:** a buffered or timed-out proxy would break long SSE
  streams. That's why it uses `FlushInterval: -1`, the stream client
  with no overall timeout, and a test that a streamed event arrives
  before the response ends.
- **Daemon load:** each agent run with an OAuth source adds goroutines
  and connections in the daemon for its duration. That's bounded by the
  number of concurrent runs.
- **Upstream 401 with a fresh-looking token** (revoked early): the proxy
  passes it through, and the next poll's refresh path marks the login
  dead as today.
- **Agents that relied on the egress allowlist including the MCP host**
  for other traffic to that host lose it. They can add it to
  `egress.allow`. Upgrade note.
- **Hosts:** none.

## Verification

- **`internal/action` tests** (`oauthproxy_test.go`), with a fake
  upstream `httptest` server:
  - a missing or wrong key gets 401, and the upstream isn't called;
  - another path gets 404;
  - the upstream sees `Bearer <token>`, the static header, and not the
    agent's `Authorization` or `Cookie`;
  - the token func is called per request, so a changed token is used on
    the next request;
  - a token error gives 401 with the message, and the upstream isn't
    called;
  - SSE: the upstream writes one event, flushes, and blocks. The client
    reads that event before the upstream finishes;
  - after `stop`, the listener is closed and the run dir is gone.
- **`internal/job` test:** an agent run (sandbox `none`, a fake agent
  command that dumps its MCP config) with an OAuth source. The config
  holds a loopback URL and a key, not the access token.
- **`internal/config` test:** `AgentEgress` leaves out an OAuth source's
  host, and still includes a bearer source's host.
- **Builds:** `go vet ./... && go test -race ./...`, then CI's
  `nix flake check`. In the VM, an agent run reaches an OAuth MCP source
  through the proxy in systemd mode, if the existing OAuth VM fixture
  allows it (decided in the plan).
