---
status: approved
issue: 102
spec: spec/2026-10-14-102-azure-devops-agent-tools.md
---

# Plan: Azure DevOps tools for agents (the local PAT path)

## Approved decisions, carried over

- **Scope:** Microsoft's local `@azure-devops/mcp`, pinned at tag
  **v2.10.0**, run under the bridge with a PAT. The remote Entra server is a
  follow-up issue: Entra's metadata issuer is a template, and it omits PKCE
  methods, so go-sdk refuses it.
- **Packaging:** Nix `buildNpmPackage` with `npmDepsHash`. No `npx` at
  runtime. Upstream's `bin` is `mcp-server-azuredevops` → `dist/index.js`.
  Upstream tests are off.
- **The wrapper `siphon-azure-devops-mcp`** is `mainProgram`, built with
  `writeShellApplication` and coreutils on its PATH. It:
  - requires `AZURE_DEVOPS_ORG` and `AZURE_DEVOPS_PAT`, with a clear error
    for each;
  - sets `PERSONAL_ACCESS_TOKEN=$(printf 'siphon:%s' "$AZURE_DEVOPS_PAT" | base64 -w0)`.
    `printf` is a builtin, so the PAT is never in argv;
  - unsets `AZURE_DEVOPS_PAT`;
  - runs `exec mcp-server-azuredevops "$AZURE_DEVOPS_ORG" -a pat "$@"`.
- **The module fallback:** `pkgs.${e.package} or self.packages.<system>.${e.package}`.
- **The catalog `azure-devops` entry:**
  - capabilities `[tools, webhooks]`;
  - `status: needs-package`, with `needs_package: [azure-devops]`;
  - `nix: {attr: azure-devops-mcp, args: [], env: [AZURE_DEVOPS_ORG, AZURE_DEVOPS_PAT], hosts: [dev.azure.com, vssps.dev.azure.com, almsearch.dev.azure.com]}`;
  - fields `organization` (text) and `pat` (secret);
  - when `organization` is set, it creates an `{{.name}}` mcp source with
    `package: azure-devops`, `env.AZURE_DEVOPS_ORG` and
    `env.AZURE_DEVOPS_PAT`, and `fail`s if the PAT is missing;
  - the `{{.name}}-hooks` source is unchanged.
- **Read-only by default.** Write tools are the `*_write` tools, plus
  `wit_work_item_attachment_upload` and `repo_create_branch`. Templates
  list only read tools: `pipelines_build`, `pipelines_build_log`,
  `pipelines_run`, `wit_work_item`, `wit_query`, `repo_pull_request`,
  `repo_pull_request_thread`, `repo_file`, `search_code` and
  `search_workitem`.
- **Verification:**
  - no live organization was given, so a smoke check, the module eval check
    and Go tests cover it;
  - the owner does the live check.
- **The connection test:** the catalog `test` can't Basic-encode a PAT,
  because `{token}` is substituted raw. So it's a Go case that reuses
  `runHTTPTest`:
  - `GET https://dev.azure.com/<org>/_apis/connectionData`;
  - header `Authorization: Basic base64(":"+pat)`;
  - identity `authenticatedUser.providerDisplayName`.

## Steps

1. **The package and the smoke check.**
   - **New file `nix/pkgs/azure-devops-mcp.nix`,** with the build and the
     wrapper above. Get the `src` and `npmDepsHash` hashes by building with
     `lib.fakeHash` first.
   - **`flake.nix:60-62`:** add
     `azure-devops-mcp = pkgs.callPackage ./nix/pkgs/azure-devops-mcp.nix { };`.
   - **`nix/aws-mcp-smoke.py`,** two optional env knobs, kept small:
     - `SMOKE_EXPECT` (comma-separated tool names that must be present);
     - `SMOKE_SECRET` (a value that must not appear in
       `/proc/<p.pid>/cmdline`, read after `initialize`).
     - The wrapper `exec`s Node, so `p.pid` is the server itself.
   - **`flake.nix:86-93`:** add `checks.azure-devops-mcp-smoke` with
     `AZURE_DEVOPS_ORG=smoke`, `AZURE_DEVOPS_PAT=smoke-pat-value`,
     `SMOKE_EXPECT=wit_work_item,pipelines_build_log` and
     `SMOKE_SECRET=smoke-pat-value`.
   - **First, check that upstream lists tools without a network call.** If
     it doesn't, drop `SMOKE_EXPECT` to a clean `initialize`, and log that
     as a deviation.
   - **Also record, in the step's report, whether upstream's HTTP stack
     honours `HTTPS_PROXY`.** Grep `azure-devops-node-api` and
     `typed-rest-client` in the built `node_modules` for proxy env handling.
     Step 5's docs state the answer.

   → verify: `nix build .#azure-devops-mcp --rebuild` and
   `nix build .#checks.x86_64-linux.azure-devops-mcp-smoke -L`.

   Traps:
   - **`vendorHash`/`npmDepsHash`:** always `--rebuild`. A stale store path
     can make a wrong hash look right.
   - The smoke check runs offline in the sandbox.
   - Never `pgrep -f`.
   - A PAT-shaped test literal must not look like a real token.

2. **The module fallback, and its eval check.**
   - **`nix/module.nix:393`:** change it to
     `package = pkgs.${e.package} or self.packages.${pkgs.stdenv.hostPlatform.system}.${e.package};`.
     Check that `self` is in scope there; `:399` already uses
     `self.packages`.
   - **`flake.nix:109-118`:** use `catalogPackages = [ "grafana" "azure-devops" ];`,
     with these asserts:
     - `pk ? azure-devops`;
     - `pk.azure-devops.package == self.packages.${pkgs.stdenv.hostPlatform.system}.azure-devops-mcp`;
     - `builtins.elem "dev.azure.com" pk.azure-devops.hosts`.

   This needs step 3's JSON entry, so step 3's `go generate` runs first, or
   the two steps land together.

   → verify: `nix build .#checks.x86_64-linux.catalog-module`.

   Traps: Nix merges use `recursiveUpdate`/`mkMerge`, never `//`.

3. **The catalog entry, `internal/catalog/services.yaml:1042-1067`.**
   - Change it as in the decisions, with field help that gives the PAT
     scopes: *Work Items: Read*, *Build: Read*, *Code: Read*, and *Read &
     write* only for write tools. Also mention the expiry.
   - Model it on Grafana's (`:584-586`) for `needs_package`, `reason` and
     `nix`, and on ServiceNow's (`:354-400`) for the conditional mcp source
     and the `fail`.
   - Add `azure-devops-build-triage` to `templates:`.
   - Update `summary` (drop "Agent tools come later.") and `setup`.
   - Run `go generate ./internal/catalog` to regenerate
     `nix/catalog-packages.json`.
   - **The render test,** in `internal/catalog/catalog_test.go`, covers
     three cases:
     - an organization and a PAT give the mcp source (`package:
       azure-devops`, the env) plus the hooks source;
     - no organization gives the hooks source only;
     - an organization with no PAT fails.

   → verify: `go test -race ./internal/catalog`.

   Traps: the generated JSON must match (`catalog_test.go:69-77`).

4. **The connection test, `internal/web/connection_test_run.go`.**
   - Add `azureDevOpsSource(cfg, names)`, like `serviceNowSource` (`:228-236`),
     but for `Package == "azure-devops"`.
   - Add a `case` next to `:115-120`:
     - mask the PAT parts **and** the base64 header value;
     - build the URL with `url.PathEscape(org)` through `testTarget`;
     - call `s.runHTTPTest(ctx, svcTest{Name: conn}, "GET", url, "Authorization", "Basic "+b64, "", "authenticatedUser.providerDisplayName")`.
   - **The test,** next to the existing ServiceNow test in `internal/web`,
     uses an httptest fake that checks the Basic header and returns the
     identity JSON. Assert that `user` is set and that neither the PAT nor
     the base64 value is in the response.

   → verify: `go test -race ./internal/web`.

   Traps: mask every secret form. The base64 value is a secret too.

5. **The template and the docs.**
   - **The new `docs/templates/azure-devops-build-triage.yaml`,** in the
     header style of `azure-devops-build-failed.yaml`:
     - `needs: sources/azure-devops-hooks, sources/azure-devops`;
     - the rule is on `eventType == "build.complete" && resource.result == "failed"`
       with `id: event.id`;
     - the agent action has `mcp: [azure-devops]`, the read-only
       `allowed_tools` (in the `mcp__azure-devops__<tool>` form; check the
       existing templates' form), `cooldown`, and a prompt to read the
       build log and the linked work items, then summarise.
   - **`cmd/siphon/describe_test.go:16`, `baseConfig`:** add an
     `azure-devops` mcp package to `server.mcp_packages`, with `command`,
     `env` and `hosts`. Then add the `azure-devops` mcp source (`package:
     azure-devops`, env from `env:` refs) next to `:59`, so
     `TestTemplatesValidate` applies it.
   - **The new `docs/connections/azure-devops.md`:**
     - the webhooks, briefly, pointing at the catalog setup;
     - "Agent tools": the PAT with its scopes and expiry, and `siphon
       connect azure-devops --organization <org> --pat -`;
     - NixOS `catalogPackages = [ "azure-devops" ]`, and `server.mcp_packages`
       elsewhere;
     - the read/write tool table;
     - the proxy finding from step 1;
     - the live check for the owner: connect, then `siphon test service
       azure-devops`, then an agent call;
     - a link to the remote follow-up issue.
   - Link the page from `docs/connections/README.md`, then run
     `go generate ./docs`.

   → verify: `go test -race ./cmd/siphon ./docs/...`.

   Traps:
   - `TestTemplatesAreCSPClean` doesn't apply here (docs only).
   - Templates are embedded, so `go generate ./docs` must leave no diff.

6. **The follow-up issue** (mine, no code). `gh issue create` "Azure DevOps:
   the remote MCP server (Entra) needs go-sdk shims", with:
   - the issuer template;
   - the missing `code_challenge_methods_supported`;
   - the app-registration steps;
   - the probe results from the intent.

   Then link it from the step 5 docs, in the same commit as step 5 or a
   follow-up.

## Deviations

- **Step 1: the build needs native-addon inputs.** Upstream loads `keytar`
  at start-up even with `-a pat`. The build adds
  `nativeBuildInputs = [pkg-config python3]`, `buildInputs = [libsecret]`
  and `npm_config_nodedir`.
- **Step 1's findings:**
  - `tools/list` works offline: 40 tools, with no fallback needed. The
    start-up tenant lookup, `dist/org-tenants.js:30`, fails and is logged.
  - **The API calls honour the proxy variables:** `typed-rest-client`
    `HttpClient.js:467-472` reads `HTTP(S)_PROXY`.
  - **The tenant lookup doesn't:** it uses Node's native `fetch`, which
    ignores them. That's harmless, because only the tenant lookup uses it.
  - Step 5's docs say this.

- **Step 5: the plan named the write tool `wit_work_item_attachment_upload`,
  which doesn't exist in v2.10.0.**
  - The only attachment tool is `wit_work_item_attachment`. It downloads,
    and with `savePath` it writes a file under a relative path (upstream
    refuses `..` and absolute paths). The docs list it as a read tool, with
    that note, and it isn't in the template.
  - `pipelines_run` is checked as read-only: its actions are only `get` and
    `list`.
- **Step 5: generator maps.** `docs/internal/gencatalog` and `genllms`
  needed the new page added to their maps, so it's linked and included in
  `llms-full.txt`.

- **The Opus review of the whole diff:**
  - **HIGH, fixed: the step 1 proxy finding was wrong.** Upstream's
    search, wiki, test-plan and some write tools also use native `fetch`,
    not just the tenant lookup. Under restricted egress (`PrivateNetwork`,
    proxy only), those tools failed. The wrapper now exports
    `NODE_USE_ENV_PROXY=1`. It was checked against a logging proxy:
    `search_code` CONNECTs to `almsearch.dev.azure.com`, and
    `core_list_projects` to `dev.azure.com` and `vssps.dev.azure.com`.
  - **MEDIUM, fixed: the triage prompt had no project.** The pipelines
    tools require one. The prompt now passes
    `resourceContainers.project.id` and the build id only.
  - **MEDIUM, fixed: the template header said "only ids reach the
    prompt",** but the definition name and build number did too. Those are
    now dropped from the prompt.
  - **LOW, fixed:**
    - the flake attribute is renamed to `siphon-azure-devops-mcp`, so a
      future nixpkgs `azure-devops-mcp` can't shadow the wrapper through the
      module's `pkgs.${x} or self.packages` fallback;
    - the connection test gains a failure case (wrong PAT gives `ok:false`,
      with no PAT or base64 value in the response).
  - **LOW, noted but not changed:**
    - the base64 PAT isn't in the bridge mask (upstream never logs it);
    - the organization field isn't validated;
    - all 40 tools are exposed, and `allowed_tools` is the boundary, per
      the spec;
    - `cooldown` on `on: each`.

## Tests

```sh
go vet ./... && go test -race ./...
go generate ./internal/catalog ./docs && git diff --exit-code
nix build .#azure-devops-mcp --rebuild
nix build .#checks.x86_64-linux.azure-devops-mcp-smoke .#checks.x86_64-linux.catalog-module
```

`nix flake check` (CI) must pass.

**Model split:** steps 1–5 edit files, so the `coder` agent does them one at
a time (with steps 2 and 3 together, see step 2). Step 6 is mine. A fresh
Opus agent reviews `git diff` against this plan.

## Rollback

Revert the PR. The module change is a pure fallback: nixpkgs lookups are
unchanged. The catalog entry returns to webhooks-only, and the package and
check go away with the revert. No state or schema migrations.
