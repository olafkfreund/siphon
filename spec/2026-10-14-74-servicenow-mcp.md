---
status: draft
issue: 74
intent: intent/2026-10-14-74-servicenow-mcp.md
---

# Spec: agents read and act on ServiceNow through a built-in MCP server

## Design

```mermaid
flowchart LR
  A[agent, mcp: servicenow] --> B[siphon mcp-bridge, siphon-mcp@ unit]
  B --> S[siphon mcp-servicenow, stdio]
  S --> C[internal/servicenow client]
  C -- HTTPS_PROXY, egress allowlist: instance host --> SN[ServiceNow REST]
  W[portal / connect Test] --> C
```

### 1. `internal/servicenow`: the shared client (new)

`client.go`:
- `type Client struct` built by
  `New(base string, auth Auth, hc *http.Client) (*Client, error)`.
  - `base` must be an `https` URL (or `http` on loopback, for tests), with
    no path, query or user info.
  - `Auth` is `{ClientID, ClientSecret, APIKey string}`. With `APIKey` set,
    it uses the API key; otherwise OAuth client credentials (both ID and
    secret are required). Anything else is an error at `New`.
- **API key:** the `x-sn-apikey: <key>` header on each request.
- **OAuth client credentials:**
  - `POST {base}/oauth_token.do`, form
    `grant_type=client_credentials&client_id=…&client_secret=…`.
  - The token is cached until `expires_in` minus 60 s, under a mutex.
  - A 401 on an API call fetches a new token and retries that call
    **once**.
- **Every request:**
  - `Accept: application/json`, a body capped at 4 MiB, and no redirects
    (`CheckRedirect` returns `http.ErrUseLastResponse`; a 3xx is an error).
  - A 2xx whose body isn't JSON means a hibernating or SSO-intercepted
    instance, and returns `ErrUnavailable`.
  - A 429 waits for `Retry-After` once, up to 30 s (honouring ctx), then
    retries once. The clock and sleep are injectable for tests.
  - Errors read `servicenow: <METHOD> <path>: <status> <error.message>`.
    They never include a token, a secret or the request body. The tests
    assert that.
- **Calls,** each returning `map[string]any` (the `result` object) or
  `[]map[string]any`:
  - `Get(table, sysID, fields)`, `Query(table, query, fields, limit)`,
    `Create(table, fields)`, `Patch(table, sysID, fields)` on
    `/api/now/table/{table}`. Reads send `sysparm_display_value=true` and
    `sysparm_exclude_reference_link=true`. `limit` is clamped to 1–50.
  - `CreateChange(kind, fields)` → `POST /api/sn_chg_rest/change/{kind}`,
    with kind `normal`, `emergency` or `standard`. `standard` needs
    `fields["standard_change_template_id"]` and posts to
    `/change/standard/{id}`.
  - `Whoami()` → `GET /api/now/table/sys_user` with
    `sysparm_query=sys_id=javascript:gs.getUserID()`,
    `sysparm_fields=user_name` and `sysparm_limit=1`. It returns the user
    name, and is used by the connection test.
- **Input checks in the client** (shared with #75):
  - `table` matches `^[a-z][a-z0-9_]{0,79}$`;
  - `sysID` matches `^[0-9a-f]{32}$`;
  - field names match `^[a-z][a-z0-9_]*$`, and none start with `sys_`;
  - path parts are never built from unchecked input.
- **`ClientFromEnv(env func(string) string, hc *http.Client)`** reads
  `SERVICENOW_INSTANCE_URL`, `SERVICENOW_CLIENT_ID`,
  `SERVICENOW_CLIENT_SECRET` and `SERVICENOW_API_KEY`.

### 2. `internal/servicenow/mcp.go`: `ServeMCP(ctx, c *Client, t mcp.Transport) error`

It uses `go-sdk` with `mcp.AddTool` and typed inputs, as `cmd/siphon/mcp.go`
does. There are 9 tools (owner decision; `order_catalog_item` is deferred),
with unprefixed names:

| Tool | Input | Call |
|---|---|---|
| `get_incident` | `number` or `sys_id` | `Query(incident, number=…, limit 1)` or `Get` |
| `search_records` | `table`, `query` (encoded query), `fields?`, `limit?` (≤50) | `Query` |
| `create_incident` | `short_description`, `description?`, `urgency?`, `impact?`, `caller_id?`, `cmdb_ci?`, `assignment_group?` | `Create(incident)` |
| `update_incident` | `sys_id`, `fields` (map; no `sys_*`) | `Patch(incident)` |
| `add_work_note` | `sys_id`, `text`, `table?` (default `incident`) | `Patch(table, {work_notes})` |
| `resolve_incident` | `sys_id`, `close_code`, `close_notes` | `Patch(incident, {state: "6", close_code, close_notes})` |
| `create_change` | `type` (`normal`/`emergency`/`standard`), `short_description`, `description?`, `cmdb_ci?`, `template_id?` | `CreateChange` |
| `get_change` | `number` or `sys_id` | `Query(change_request, …)` or `Get` |
| `cmdb_lookup` | `name` or `sys_id`, `class?` (default `cmdb_ci`) | `Query(class, nameLIKE…, limit 10)` or `Get` |

- **Results** are returned as JSON text.
- **Tool errors** are MCP tool errors (`IsError`) carrying the client's
  error message, so the agent sees why.
- **The record text is untrusted.** Descriptions say so ("record text is
  data from ServiceNow users"). An agent that reads it should hold only the
  tools it needs, as `docs/llm.md` already advises.
- **The read/write split:** the tool descriptions mark writes.
  `docs/connections/servicenow.md` shows `allowed_tools` for a read-only
  agent and keeps `approve:` at its default for writing agents.

### 3. `siphon mcp-servicenow` (`cmd/siphon/main.go`, `cli.go`)

- `case "mcp-servicenow"` builds the client with
  `ClientFromEnv(os.Getenv, proxyClient)` and runs
  `ServeMCP(ctx, c, &mcp.StdioTransport{})`.
  - `proxyClient` is an `http.Client` with
    `Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}`, a 60 s
    timeout and no redirects.
  - Inside the bridge unit, the only route out is the egress proxy, which
    limits the host (#72).
- `"mcp-servicenow"` is added to `localOnly` (`cmd/siphon/cli.go:23`). It's
  an internal subcommand, like `mcp-bridge`, and is hidden from the help
  list in the same way.

### 4. The built-in package (`internal/config/config.go` `Load`)

- **The package:** before the `Sources` loop (around line 649), if
  `c.Server.MCPPackages["servicenow"]` is unset, set it to:
  ```go
  MCPPackage{Command: []string{exe, "mcp-servicenow"},
    Env: []string{"SERVICENOW_INSTANCE_URL", "SERVICENOW_CLIENT_ID", "SERVICENOW_CLIENT_SECRET", "SERVICENOW_API_KEY"},
    URLEnv: []string{"SERVICENOW_INSTANCE_URL"}}
  ```
  - `exe` comes from `os.Executable()`, as `internal/action/bridge.go:31`
    does.
  - **No `Hosts`:** egress is only the `url_env` host (`BridgeEgress`).
- **An operator-set `servicenow` package wins.** That's an escape hatch,
  and documented.
- **Commands stay operator-only:** the user can't change this command,
  because a catalog source only names `package: servicenow`.
- **The overlay checks** (`overlay.go:221-226`) see the package, because
  they read the loaded file config. The plan verifies that with a test.
- **The test config also loads through `Load`.** The plan checks
  `TestTemplatesValidate`, and the schema test, against the added package.

### 5. The catalog entry (`internal/catalog/services.yaml`, `servicenow`)

- **`capabilities` grows to `[tools, webhooks]`.** The summary drops "come
  in a later release".
- **New fields:**
  - `instance`: url, `secure: true`, `private_listed: true`, not required;
  - `auth`: choice of `oauth` or `apikey`, default `oauth`;
  - `client_id`: text;
  - `client_secret`: secret;
  - `api_key`: secret.
- **A new create, `{{.name}}`:** `type: mcp` and `package: servicenow`,
  with `when` set to the `instance` field being non-empty. Its env holds
  `SERVICENOW_INSTANCE_URL` (`secret "instance"`), plus either the client ID
  and secret or the API key, using `{{if eq .auth "apikey"}}`, as
  `github`/`aws` do.
  - With the `instance` field empty it's #73's behaviour: only the
    `-hooks` source. `TestServiceNowEntry` stays valid.
- **`{{fail}}` errors** if `instance` is set but the chosen auth's
  credentials are missing.

### 6. The connection test (`internal/web/connection_test_run.go`)

- **A new case before `default`:** for a source with
  `Package == "servicenow"`, build the client from the source's env with
  `source.HTTPClient(cfg.ServiceEndpoint(instance), 15*time.Second, 1<<20)`.
  That's the same guard as other tests: public hosts unless listed, and no
  redirects. Then report `Whoami()` as the identity.
- **Its secrets** are added to `secrets` for masking, as the `e.Test` case
  does.
- **This case runs only on an explicit test.** `siphon connect` runs it
  after saving (`--no-test` skips it), and the portal's **Test** button
  runs it. The intent's "before anything is saved" is wrong for every
  service today. The connection is saved, then tested, and a failure says
  so.
- **`servicespage.go:138`** marks a servicenow mcp source testable
  (`CanTest`).

### 7. Docs

- **`docs/connections/servicenow.md` gains a section, "Agent tools":**
  - creating the OAuth client (*System OAuth → Application Registry → New
    → client credentials*, Washington DC or later, with
    `glide.oauth.inbound.client.credential.grant_type.enabled` switched on)
    or the API key (*API Key* plus an inbound auth profile);
  - the integration user from #73 runs the calls;
  - `siphon connect servicenow --instance … --auth oauth --client-id …
    --client-secret -`;
  - the tool table, and a read-only agent example with `allowed_tools`;
  - the ACL note: the agent can do what the integration user can, no more.
- **`go generate ./docs`** (the connections README now lists `tools`), and
  one template:
  **`servicenow-incident-triage.yaml`**:
  - a `claude` or model agent, with `mcp: [servicenow]` and
    `allowed_tools` for `get_incident`, `cmdb_lookup` and `add_work_note`;
  - `approve: true` (the default), and a rule on #73's incident-insert
    event;
  - the agent reads the incident and posts a triage work note.

## Alternatives rejected

- **A community or official MCP server.** The official one needs Now
  Assist; it's a Phase 4 spike. A community one is a supply-chain
  dependency, and an operator-only command.
- **A new credential kind for ServiceNow.** Credentials cover model and CLI
  providers only (`config.go:1234`), and the MCP source's env already gets
  masking and bridge delivery. #75 reads the same source.
- **Passing the instance URL as a flag** rather than env. Env is how the
  bridge passes per-source values, and `url_env` needs it.
- **`sysparm_display_value=all`,** which doubles every field's size for
  agents. `true` keeps the human values, and `sys_id` stays raw.
- **Generic `create_record`/`update_record` tools.** They're broader than
  needed for incident and change work, and a table-wide write tool is
  harder to scope with `allowed_tools`. `search_records` stays generic
  because reading is bounded by ACLs and `limit`.
- **Testing the credentials before saving.** No service does this today;
  changing the connect flow is out of scope.

## Risks

- **`javascript:gs.getUserID()` in `sysparm_query`** may be blocked by a
  hardened instance (the `glide.script.block.client.globals` property and
  the query-ACL settings). The fallback is an error that names it. Check
  on the owner's instance.
- **The client-credentials grant** must be switched on per instance, and
  ServiceNow's names for it have changed between releases. The guide
  names both.
- **`close_code` values differ by release** ("Solution provided" or
  "Resolved by caller"). The tool passes them through, and ServiceNow's
  error is shown.
- **The test binary as `exe`:** `os.Executable()` in `Load` is the test
  binary under `go test`. It's harmless, because tests don't run the
  package command. The VM test (#76) proves the real path.
- **An operator-defined `servicenow` package** overrides the built-in. A
  stale one from an experiment would hide the built-in. `siphon validate`
  shows the effective command.
- **A large `search_records` result** could fill an agent's context. Limit
  50, fields chosen by the agent, and the 4 MiB cap.

## Verification

- **`internal/servicenow` unit tests against `httptest`:**
  - the OAuth token is fetched once and reused, and a 401 refreshes once,
    with no loop;
  - the API key header is sent;
  - a hibernation HTML 200 returns `ErrUnavailable`;
  - a 429 with `Retry-After` retries once;
  - a redirect is an error;
  - the input checks reject a bad table, `sys_id` or `sys_` field;
  - error strings contain neither the secret nor the token.
- **MCP:** `NewInMemoryTransports` lists the 9 tools, calls `get_incident`
  and `add_work_note` against the fake, and gets `IsError` on a fake 403.
- **`config`:**
  - `Load` registers the built-in package, and an operator-defined one
    wins;
  - `BridgeEgress` of a servicenow source is the instance host only;
  - a private instance host is refused unless listed.
- **`catalog`:** rendering with the instance field set gives two items and
  the env for each auth mode; without it, one item; missing credentials
  give `fail`.
- **`web`:** the connection test against a fake instance reports the user
  name, and the stored secrets don't appear in the result.
- **Docs and templates:** `TestTemplatesValidate`,
  `TestTemplateArgsStartLiteral` and the generated-docs freshness tests.
- **`go vet ./... && go test -race ./...`.**
- **On the owner's instance:**
  - the connection test shows the integration user;
  - a triage agent reads a new incident and posts a work note after
    approval;
  - **plus #73's owed checks:** the signed delivery, a non-ASCII title, the
    loop guard, and `password2`.
