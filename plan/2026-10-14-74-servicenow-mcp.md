---
status: approved
issue: 74
spec: spec/2026-10-14-74-servicenow-mcp.md
---

# Plan: agents read and act on ServiceNow through a built-in MCP server

## Approved decisions

These are carried over from the spec and the intent.

- **A new package, `internal/servicenow`.**
  - **The client:** OAuth client credentials (`POST {base}/oauth_token.do`;
    the token is cached until `expires_in` minus 60 s; a 401 refreshes and
    retries once), or `x-sn-apikey` when an API key is set.
    - Every request is JSON with a body capped at 4 MiB.
    - It never follows a redirect (a 3xx is an error).
    - A non-JSON 2xx returns `ErrUnavailable`.
    - A 429 retries once after `Retry-After` (≤30 s), with an injectable
      sleep.
    - Errors read `servicenow: <METHOD> <path>: <status> <error.message>`,
      never a secret.
  - **Reads** use `sysparm_display_value=true` and
    `sysparm_exclude_reference_link=true`, with `limit` clamped to 1–50.
  - **Input checks:**
    - table `^[a-z][a-z0-9_]{0,79}$`;
    - `sys_id` `^[0-9a-f]{32}$`;
    - field names `^[a-z][a-z0-9_]*$`, and not `sys_*`.
  - **Env:** `SERVICENOW_INSTANCE_URL`, `SERVICENOW_CLIENT_ID`,
    `SERVICENOW_CLIENT_SECRET`, `SERVICENOW_API_KEY`.
- **9 unprefixed MCP tools:** `get_incident`, `search_records`,
  `create_incident`, `update_incident`, `add_work_note`, `resolve_incident`,
  `create_change`, `get_change`, `cmdb_lookup`. `order_catalog_item` is
  deferred.
  - Tool errors are `IsError` results.
  - Descriptions mark writes and say that record text is untrusted.
- **`siphon mcp-servicenow`** is internal and `localOnly`, and serves
  stdio. Its HTTP client uses `http.ProxyFromEnvironment`, a 60 s timeout
  and no redirects.
- **A built-in package `servicenow`,** registered in `config.parse` unless
  the operator defines one:
  - command `[os.Executable(), "mcp-servicenow"]`;
  - the 4 env names;
  - `url_env: [SERVICENOW_INSTANCE_URL]`;
  - no `hosts`.
- **The catalog:**
  - `capabilities: [tools, webhooks]`;
  - optional fields `instance` (url, `secure`, `private_listed`), `auth`
    (`oauth`/`apikey`, default `oauth`), `client_id`, `client_secret` and
    `api_key`;
  - creates `{{.name}}` (`type: mcp`, `package: servicenow`) only when
    `instance` is set, and `fail`s on missing credentials;
  - `{{.name}}-hooks` is unchanged.
- **The connection test:** a Go case for `Package == "servicenow"`, using
  `source.HTTPClient(cfg.ServiceEndpoint(instance), 15s, 1 MiB)` and
  `Whoami()` (`sys_user` with `sys_id=javascript:gs.getUserID()`).
  - Its secrets are masked.
  - It runs after saving, as for every service.
- **Docs:**
  - an "Agent tools" section in `docs/connections/servicenow.md`;
  - a template, `servicenow-incident-triage`: an agent with
    `allowed_tools` for `get_incident`, `cmdb_lookup` and `add_work_note`,
    keeping its default approval.
- **Rejected:**
  - a community or official server (the official one is #76's spike);
  - a credential kind;
  - generic write tools;
  - `display_value=all`;
  - testing before saving.

## Steps

1. **`internal/servicenow/client.go` and `client_test.go` (new).**
   - **Tests first,** against `httptest.NewServer`:
     - the OAuth token is fetched once across two calls;
     - a 401 then 200 refreshes once, and a 401 then 401 errors with no
       loop;
     - the API key sends `x-sn-apikey` and never calls `/oauth_token.do`;
     - an HTML 200 is `ErrUnavailable`;
     - a 429 with `Retry-After: 2` calls the injected sleep with 2 s and
       then retries;
     - a 302 is an error;
     - bad table, `sys_id` and `sys_*` field inputs are rejected before any
       request;
     - every error string contains neither the secret, the API key nor the
       token. Use distinctive values such as `sekrit-…`.
   - **Then the code:**
     - `New`, `ClientFromEnv`, `Get`, `Query`, `Create`, `Patch`,
       `CreateChange` and `Whoami`;
     - `Client.sleep func(ctx, time.Duration) error` defaults to a
       ctx-aware timer.
   - **Verify:** `go test -race ./internal/servicenow`.
   - **Traps:**
     - Never put a secret into an `error` or a log.
     - `url.PathEscape` isn't a substitute for the regex checks.
     - Build queries with `url.Values`.
     - Inject time; no real sleeps in tests.
     - stdlib only.

2. **`internal/servicenow/mcp.go` and `mcp_test.go` (new).**
   - `ServeMCP(ctx, c *Client, t mcp.Transport) error`, with the 9 tools
     via `mcp.AddTool` and typed input structs (`jsonschema` tags with
     descriptions). Follow `cmd/siphon/mcp.go:118-150` for the go-sdk
     v1.8.0 API.
   - The tools map to client calls per the spec's table. The results are
     JSON text.
   - **Test** with `mcp.NewInMemoryTransports()` (see
     `internal/source/mcp_test.go:18`):
     - `ListTools` returns exactly the 9 names;
     - `get_incident` by number hits
       `/api/now/table/incident?sysparm_query=number=INC…`;
     - `add_work_note` PATCHes `{"work_notes": …}`;
     - a fake 403 gives `IsError` with the status;
     - `resolve_incident` sends `state: "6"`.
   - **Verify:** `go test -race ./internal/servicenow`.
   - **Traps:**
     - A go-sdk tool handler returning a Go error becomes a protocol error.
       Return `IsError` results instead, so the agent sees the reason.
     - `get_incident`'s number goes into the encoded query. Check it
       against `^[A-Z]{2,8}[0-9]{4,12}$` so it can't add `^` clauses.

3. **`cmd/siphon/main.go` and `cli.go`.**
   - Add `case "mcp-servicenow"` next to `mcp-bridge` (`main.go:78`).
     - Build the `http.Client`
       (`Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}`,
       `Timeout: 60s`, `CheckRedirect` returns `http.ErrUseLastResponse`).
     - Call `servicenow.ClientFromEnv(os.Getenv, hc)`, then `ServeMCP` on
       `&mcp.StdioTransport{}`.
     - On error, print `mcp-servicenow: <err>` to stderr and exit 1.
   - Add `"mcp-servicenow"` to `localOnly` (`cli.go:23`).
   - **Verify:**
     - `go build ./cmd/siphon`;
     - `SERVICENOW_INSTANCE_URL= go run ./cmd/siphon mcp-servicenow </dev/null`
       exits 1 with an error naming the missing variable and no secret.
   - **Traps:**
     - Don't add it to the CLI help or `docs/cli.md`: it's internal, like
       `mcp-bridge`.
     - If `TestCLIDocFresh` or a similar test lists commands, report it
       rather than edit the test.

4. **`internal/config/config.go`, `parse`: the built-in package.**
   - Before the `Sources` loop (`config.go:649`), if
     `c.Server.MCPPackages["servicenow"]` is unset, create the map if it's
     nil and set the package per the decisions. Take `exe` from
     `os.Executable()` and ignore its error, as `bridgeCommand` does.
   - **Tests** in `config_test.go`:
     - a config with a `package: servicenow` source and no `mcp_packages`
       validates;
     - its `Command` ends in `mcp-servicenow`;
     - `BridgeEgress` is only the instance host:port;
     - a `10.0.0.5` instance is refused unless it's in
       `server.services.private_endpoints`;
     - an operator-defined `servicenow` package wins.
   - **Verify:**
     - `go test ./internal/config`, including the schema test (no
       `UPDATE_SCHEMA`; the schema shouldn't change);
     - then `go test ./internal/web ./cmd/siphon ./docs`, because of the
       inventory output and the overlay checks (`overlay.go:221-226`).
   - **Traps:**
     - **Inventory and `siphon config` may now show a `servicenow`
       package.** That's expected. If a golden test fails on it, update
       that golden file only, and log it as a deviation.
     - Don't register it in `Server` defaults written back to YAML (the
       overlay save). Check that `siphon config` / overlay save doesn't
       persist it. If it does, stop and report.

5. **`internal/catalog/services.yaml` and `catalog_test.go`.**
   - In the `servicenow` entry:
     - `capabilities: [tools, webhooks]`, and a summary without "later
       release";
     - the fields above, with help text;
     - a new first create `{{.name}}`, with `when` set to whether the
       `instance` field is non-empty (match how other entries test a
       non-empty field: grep `when:`);
     - env via `secret`, with `{{if eq .auth "apikey"}}` and `{{fail}}` for
       missing credentials (see `services.yaml:27,128`);
     - setup text that mentions the Agent tools section.
   - **Extend `TestServiceNowEntry`** with table cases:
     - name only gives 1 item (unchanged);
     - `instance` plus `oauth` with an ID and secret gives 2 items and env
       keys `SERVICENOW_CLIENT_ID`/`_SECRET`, with no `API_KEY`;
     - `instance` plus `apikey` gives `SERVICENOW_API_KEY` only;
     - `instance` plus `oauth` with no secret is an error.
   - **Verify:** `go test ./internal/catalog ./internal/web`
     (`TestCatalogEntriesRenderAndCommit` and the fake-server test still
     pass).
   - **Traps:** no `needs_package`: the package is built in. Make sure
     `TestCatalogEntriesRenderAndCommit`'s default form still renders. If
     it now fills `instance` and fails, report it.

6. **`internal/web/connection_test_run.go` and `servicespage.go`.**
   - In `runConnectionTest`'s switch, before `default`: if any source in
     `names` has `Package == "servicenow"`:
     - build the client from its env with
       `source.HTTPClient(cfg.ServiceEndpoint(instance), 15*time.Second, 1<<20)`;
     - add the client secret and API key to `secrets` (via `maskParts`);
     - set `t.User` from `Whoami()`, and `t.Err` from the error.
   - In `servicespage.go:137`, set `CanTest` for such a source.
   - **Test** (the closest existing `runConnectionTest` test file): a fake
     instance on loopback, listed as a private endpoint.
     - It answers `/oauth_token.do` and the `sys_user` query.
     - The result's `User` is the user name.
     - With the fake returning 401, `Err` doesn't contain the secret.
   - **Verify:** `go test -race ./internal/web`.
   - **Traps:**
     - The fake is loopback, so the test config must list it in
       `private_endpoints`. Don't weaken the guard.
     - The CSP rules apply if any template changes (none expected).

7. **Docs and template.**
   - **`docs/connections/servicenow.md`:** an "Agent tools" section per the
     spec:
     - creating the OAuth client and the API key;
     - the `siphon connect` example, with secrets via `-` (stdin);
     - the tool table, and a read-only `allowed_tools` example;
     - the ACL note;
     - the `close_code` and `javascript:` caveats.
   - **`docs/templates/servicenow-incident-triage.yaml`** (category "Ops &
     monitoring"). Copy an existing MCP agent template's structure (for
     example `cloudflare-notification.yaml`).
     - Agent `servicenow-incident-triage`, with `mcp: [servicenow]` and
       `allowed_tools: [mcp__servicenow__get_incident, mcp__servicenow__cmdb_lookup, mcp__servicenow__add_work_note]`.
     - Default `approve`, and `cooldown`.
     - Rule `servicenow-incident-triage` on `servicenow-hooks` incident
       inserts, with `on: each` and `id: event.sys_id`.
     - The prompt passes only `{{.event.number}}` and `{{.event.sys_id}}`.
   - **`cmd/siphon/describe_test.go` `baseConfig`:** add
     `servicenow: { type: mcp, package: servicenow, env: { SERVICENOW_INSTANCE_URL: env:AGW_HOOK, SERVICENOW_API_KEY: env:AGW_HOOK } }`.
     Check the `allowed_tools` name format against an existing template.
   - Add a `docs/templates/README.md` row, add the template to the catalog
     entry's `templates:`, and run `go generate ./docs`.
   - **Verify:** `go test ./docs ./cmd/siphon ./internal/catalog`.
   - **Traps:**
     - No argument starts with `{{`, and no hand-edited generated files.
     - The `allowed_tools` prefix format must match what the agent code
       expects (grep `mcp__` in `internal/action`).

8. **All checks.**
   - `go vet ./... && go test -race ./...`.
   - Spot-check the CLI:
     - `go run ./cmd/siphon connect servicenow --help` lists `instance`,
       `auth`, `client-id`, `client-secret` and `api-key`;
     - `go run ./cmd/siphon template servicenow-incident-triage` prints the
       template.
   - `go.mod` is unchanged, so there's no `vendorHash` work. CI's VM test
       runs, but this phase adds no VM case; that's #76.
   - **Traps:** none.

## Tests

```sh
go test -race ./internal/servicenow
go test ./internal/config ./internal/catalog ./internal/web ./docs ./cmd/siphon
go vet ./... && go test -race ./...
```

**On the owner's instance** (before the PR leaves draft):
- the connection test shows the integration user;
- the triage agent reads a new incident and, after approval, posts a work
  note;
- **#73's owed checks:** the signed delivery, a non-ASCII title, the loop
  guard, and `password2`.

## Rollback

Revert the PR.
- Sources created with `package: servicenow` then fail validation (unknown
  package) until they're removed. The `-hooks` webhook sources keep
  working.
- No store, migration or schema change.

## Implementation

This is 8 steps over about 14 files, so a `coder` agent writes steps 1–7
and a fresh Opus agent reviews them. Step 8 and the instance check are
mine.
