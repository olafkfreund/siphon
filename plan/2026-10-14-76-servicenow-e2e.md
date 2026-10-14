---
status: approved
issue: 76
spec: spec/2026-10-14-76-servicenow-e2e.md
---

# Plan: ServiceNow end to end in CI, and the official ServiceNow MCP server

## Approved decisions

- **The polling template is dropped.**
- **A. The VM test:** the whole chain with a stub.
  - **The stub,** `nix/stub-servicenow.py` on `external:8081` (stdlib):
    - `POST /oauth_token.do` (client credentials, checking the stub ID and
      secret) returns `stub-tok-…`;
    - `PATCH /api/now/table/incident/<32 hex>` (the Bearer must equal the
      token) records `{method, path, body, bearer_ok}`, never the token, and
      returns `{"result":{"number":"INC0000001","sys_id":…}}`;
    - `GET /_seen` lists the records, and anything else gets 404.
  - **On `machine`:**
    - `external:8081` is added to `server.services.private_endpoints`;
    - a `servicenow` mcp source (`package: servicenow`) whose env file refs
      come from `/etc/siphon/sn-*`;
    - `servicenow-hooks` (`standard-webhooks`, a `whsec_` key in a file);
    - a rule `sn-note` (`event.table == "incident"`, `on: each`,
      `id: sys_id:sys_mod_count`, `cooldown: 1m`, action
      `servicenow: add_work_note` with `text: "seen {{.event.number}}"`).
  - **The test:** sign the delivery as the #73 Business Rule does, then
    assert:
    - one PATCH with `{"work_notes":"seen INC0000001"}` and
      `bearer_ok`;
    - the job output has `servicenow add_work_note incident INC0000001`;
    - no secret or token in the job output or the journal;
    - a resend is a duplicate, with no second PATCH.
- **B. The official MCP spike,** in a single step, done with the owner:
  - **Setup:** a "Siphon MCP" OAuth app on `calitiiltddemo3`
    (authorization code, with siphon-dev's callback), its credentials in
    agenix, and an mcp source with `url` set to the Quickstart server and
    `auth.oauth {client_id, client_secret, issuer}`. The owner logs in.
  - **If it works:** the catalog gains an `mcp_server` choice (builtin or
    official).
  - **If it fails:** the failure is documented, and a follow-up issue
    covers the fix. No catalog change.
- **C. Docs:** "Which path when", plus the spike's findings.

## Steps

1. **`nix/stub-servicenow.py` (new).**
   - Per the decisions, modelled on `nix/stub-model.py`. It reads the
     client ID and secret from env (`STUB_SN_CLIENT_ID`,
     `STUB_SN_CLIENT_SECRET`).
   - **Verify:** `python3 -m py_compile nix/stub-servicenow.py`, plus a
     quick local run with curl (token, PATCH, `_seen`).
   - **Traps:**
     - Never store or print the token or the secret in `_seen`; store a
       `bearer_ok` boolean only.
     - Use stdlib only.

2. **`nix/vm-test.nix`.**
   - **On `nodes.external`:** a `systemd.services.stub-servicenow`, like
     `stub-model` (line ~36), on port 8081, with its env set to fixed test
     values.
   - **On `nodes.machine`:**
     - `environment.etc` files `siphon/sn-url` (`http://external:8081`),
       `siphon/sn-client-id`, `siphon/sn-client-secret` and
       `siphon/sn-hook` (a `whsec_` key);
     - `private_endpoints` gains `external:8081`, merged with
       `lib.recursiveUpdate`;
     - the sources and the rule per the decisions.
   - **The test script:** a new subtest after the existing Standard
     Webhooks one (around line 744), reusing its signing approach.
     - The body is
       `{"event":"insert","table":"incident","sys_id":"<32 hex>","number":"INC0000001","sys_mod_count":"0",…}`.
     - Wait with `wait_job("sn-note", "done")`, then check the four
       assertions. The leak check greps the journal and the `siphon jobs
       show` output for the secret and `stub-tok`.
   - **Verify:** `nix build .#checks.x86_64-linux.vm -L` (or the name the
     flake uses: check `nix flake show`). Needs KVM.
   - **Traps:**
     - Nix merges: `lib.recursiveUpdate`/`mkMerge`, not `//`, for nested
       settings.
     - `pkill -f` matches its own shell, so use PIDs.
     - Python f-strings inside Nix `''` strings need `''${` escaping.
     - Keep the subtest's timeouts generous; CI is slower.

3. **The official-MCP spike (mine, with the owner).**
   - Create the OAuth app with the admin REST API (as for #74's app): name
     "Siphon MCP", authorization-code grant, redirect URL = siphon-dev's
     OAuth callback (find the path in `internal/mcpoauth`). Store the
     client ID and secret in agenix (`servicenow-calitti-env.age`, as
     `SERVICENOW_MCP_CLIENT_ID` and `SERVICENOW_MCP_CLIENT_SECRET`).
   - Add the mcp source to siphon-dev, then ask the owner to press Log in
     and consent.
   - Record the outcome under Deviations, either way.
   - **Traps:**
     - siphon-dev's `public_url` must match the redirect URL. Check it
       first.
     - Never print the client secret.

4. **Only if the spike works:** the catalog `mcp_server` choice.
   - In `internal/catalog/services.yaml` `servicenow`, a `mcp_server`
     field (choice: `builtin` or `official`, default `builtin`), plus
     `mcp_url`, `mcp_client_id` and `mcp_client_secret` when official.
     The mcp source YAML branches on the choice.
   - Extend `TestServiceNowEntry` for both choices.
   - **Verify:** `go test ./internal/catalog ./internal/web`.

5. **Docs.**
   - `docs/connections/servicenow.md` gets "Which path when" (the spec's
     table) and "The official MCP server" (the findings table and the
     spike's outcome).
   - One line in `docs/llm.md`, a `docs/README.md` link if missing, then
     `go generate ./docs`.
   - **Verify:** `go test ./docs`.

6. **All checks (mine).**
   - `go vet ./... && go test -race ./...`, and the VM check from step 2.

## Tests

```sh
python3 -m py_compile nix/stub-servicenow.py
nix build .#checks.x86_64-linux.vm -L
go vet ./... && go test -race ./...
```

## Rollback

Revert the PR. It's test, docs and, optionally, catalog only. The spike's
OAuth app on the demo instance can be deleted by hand.

## Implementation

A `coder` agent does steps 1, 2, 4 and 5, which edit about 6 files. Steps 3
and 6 are mine. Step 3 needs the owner for the login, and step 4 only runs
if step 3 succeeds. A fresh Opus agent reviews before the PR.
