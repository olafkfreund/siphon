---
status: approved
issue: 102
intent: intent/2026-10-14-102-azure-devops-agent-tools.md
---

# Spec: Azure DevOps tools for agents (the local PAT path)

**Scope** (intent decision 1): Microsoft's local `@azure-devops/mcp` server,
pinned and built by Nix, run under the bridge with a PAT. The remote Entra
path gets its own follow-up issue.

## Design

### 1. The package, `nix/pkgs/azure-devops-mcp.nix`

- **The build:** `buildNpmPackage` from the GitHub tag `v2.10.0` of
  `microsoft/azure-devops-mcp`, with `npmDepsHash` pinned. Upstream's
  `build` script runs, which is `tsc` to `dist/`. Upstream tests are off,
  because they need the network. A smoke check covers start-up (§5).
- **The program:** `mainProgram` is a small wrapper,
  `siphon-azure-devops-mcp`, built with `writeShellApplication` and
  coreutils on its PATH. It:
  - reads `AZURE_DEVOPS_ORG` and `AZURE_DEVOPS_PAT` (both required, with a
    clear error if either is missing);
  - exports `PERSONAL_ACCESS_TOKEN` as base64 of `siphon:$AZURE_DEVOPS_PAT`.
    Upstream wants base64 of `email:pat`, and any non-empty email works,
    since it splits on the first `:`;
  - unsets `AZURE_DEVOPS_PAT`;
  - runs `exec mcp-server-azuredevops "$AZURE_DEVOPS_ORG" -a pat "$@"`.

  The PAT never appears in argv. `printf` is a bash builtin, and the value
  reaches `base64` on stdin. The org, which isn't secret, is in argv.
- **The flake:** it exposes the package as `packages.<system>.azure-devops-mcp`.

**Why a wrapper:**
- Upstream takes the org as a positional argument, but a catalog package's
  `args` are static.
- Upstream wants the PAT pre-encoded.

The wrapper keeps both out of the daemon. The alternative, a templated
args or env feature in Go, is rejected below.

### 2. The NixOS module

`nix/module.nix:393` resolves a catalog package with `pkgs.${e.package}`,
which only covers nixpkgs. Change it to:

```nix
package = pkgs.${e.package} or self.packages.${pkgs.stdenv.hostPlatform.system}.${e.package};
```

Then `services.siphon.catalogPackages = [ "azure-devops" ]` installs the
pinned server, exactly as `"grafana"` does.

### 3. The catalog entry, `internal/catalog/services.yaml`, `azure-devops`

The `azure-devops` entry changes as follows:

- **`capabilities`:** `[tools, webhooks]`.
- **`status`:** `needs-package`, with `needs_package: [azure-devops]` and a
  `reason` like Grafana's. The webhooks keep working without the package.
- **The package:**

  ```yaml
  nix: {attr: azure-devops-mcp, args: [], env: [AZURE_DEVOPS_ORG, AZURE_DEVOPS_PAT],
        hosts: [dev.azure.com, vssps.dev.azure.com, almsearch.dev.azure.com]}
  ```

  These hosts are every API host upstream's source calls: org APIs,
  identities, and code, wiki and work-item search. The legacy
  `{org}.visualstudio.com` isn't needed, because upstream builds every URL
  from `https://dev.azure.com/<org>`.
- **New fields:**
  - `organization` (text; the `dev.azure.com/<org>` name; empty means
    webhooks only);
  - `pat` (secret; help text gives the scopes: *Work Items: Read*, *Build:
    Read*, *Code: Read*, plus *Write* only for write tools).
- **`creates`:** when `organization` is set, an `{{.name}}` mcp source with:
  - `package: azure-devops`;
  - `env.AZURE_DEVOPS_ORG`;
  - `env.AZURE_DEVOPS_PAT`, via `{{secret "pat" …}}`;
  - a `fail` if the PAT is missing.

  The existing `{{.name}}-hooks` webhook source is unchanged.
- **A connection test:** if the catalog's built-in test can do HTTP Basic,
  it runs `GET https://dev.azure.com/<org>/_apis/projects?api-version=7.1&$top=1`
  with the PAT. Otherwise a Go case runs it in `internal/web/connection_test_run.go`,
  as `testServiceNow` does. Which one is decided when the plan reads
  `render.go`.
- **The regenerated file:** `nix/catalog-packages.json`
  (`go generate ./internal/catalog`).

### 4. Read-only by default (intent decision 3)

The local server has no read-only switch. Read-only is enforced by the
agent's `allowed_tools`, the same model ServiceNow uses.

- Upstream names every write tool `*_write`, plus
  `wit_work_item_attachment_upload` and `repo_create_branch`.
- The templates list only read tools:
  - `pipelines_build`, `pipelines_build_log` and `pipelines_run`;
  - `wit_work_item` and `wit_query`;
  - `repo_pull_request`, `repo_pull_request_thread` and `repo_file`;
  - `search_code` and `search_workitem`.
- The docs say which tools write, and that turning them on means adding
  them to `allowed_tools`. The agent's approval gate still applies.

### 5. Verification without an organization (intent decision 2)

- **The smoke check `checks.azure-devops-mcp-smoke`:** a copy of the
  `aws-mcp-smoke` pattern (`flake.nix:86-93`). It starts the wrapper over
  stdio with a fake org and PAT, offline, and lists tools. It asserts that
  `wit_work_item` and `pipelines_build_log` are present.
  - **Checked first, in the plan:** upstream registers its tools at start-up
    without calling the API. If it doesn't, the check asserts a clean
    `initialize` instead.
- **The wrapper has no PAT leak:** the smoke check asserts the fake PAT
  isn't in the child's `/proc/<pid>/cmdline`. That's done in Python with
  the PID, never with `pgrep -f`.
- **`catalog-module`:** the existing eval check (`flake.nix:94-120`) also
  lists `"azure-devops"`, which proves the module fallback resolves
  `self.packages`.
- **Go:** a catalog render test for `azure-devops`, covering three cases:
  - with an organization and a PAT, it creates the mcp source and the hooks
    source;
  - without an organization, it creates the hooks source only;
  - with an organization but no PAT, it fails.

  `TestTemplatesValidate` covers the new template.
- **Live:** the owner runs it against a real organization. The docs give
  the three commands to run.

### 6. Template and docs

- **The template `azure-devops-build-triage`:**
  - it fires on an Azure DevOps **Build completed** webhook with
    `resource.result == "failed"`;
  - the agent, with `mcp: [azure-devops]` and the read-only tools above,
    reads the build log and the linked work items, then writes a summary;
  - `cooldown` is set;
  - it's listed in the entry's `templates`.
- **`docs/connections/azure-devops.md`** gets an "Agent tools" section:
  - creating a PAT, with its scopes and expiry;
  - connecting with `siphon connect azure-devops --organization <org> --pat -`;
  - `catalogPackages` on NixOS, and `server.mcp_packages` elsewhere;
  - the read/write tool table;
  - a pointer to the remote-server follow-up issue.
- Then `go generate ./docs`.

## Alternatives rejected

- **`npx @azure-devops/mcp` at runtime.** It fetches unpinned code over the
  network, and the bridge's egress would have to allow npm. Constraint:
  pinned and reproducible.
- **Templated `args` or env encoding in Go** (`{env.X}` in package args, a
  `base64` env transform). That's a new general feature in the bridge for
  one package. The Nix wrapper does it in a few lines, at the edge.
- **`-a envvar` with `ADO_MCP_AUTH_TOKEN`.** Upstream sends that as a
  Bearer token. A PAT needs Basic, which is what `-a pat` does.
- **Asking the user for the base64 value.** It's error-prone, and the
  intent says Siphon does the encoding.
- **The remote Entra server now.** It's deferred by decision 1, and it
  needs go-sdk shims: the issuer template, and the PKCE metadata Entra
  omits.

## Risks

- **`npmDepsHash` drift.** Upstream's lock file pins it. Re-check with
  `nix build --rebuild` (the AGENTS.md trap for hashes).
- **Upstream changes tool names.** Pinning the tag freezes them. A bump
  re-checks the template's `allowed_tools`, and the smoke check's asserted
  names catch a rename.
- **Bridge egress:**
  - under restricted egress, a call to a host not in `hosts` fails;
  - the three hosts come from upstream's source, and a version bump
    re-greps them;
  - the Node HTTP client must honour the proxy variables that #72 passes.
    I don't know yet whether upstream's `azure-devops-node-api` does. The
    plan checks it in the VM, or documents `sandbox: none`.
- **Closure size:** Node plus about 200 npm packages, only on hosts that
  list the package.
- **Hosts affected:** only those that list it in `catalogPackages`. The
  module change is a fallback, so the existing nixpkgs lookups are
  unchanged.

## Verification

- `go vet ./... && go test -race ./...`, plus `go generate ./internal/catalog ./docs`
  with no diff afterwards.
- `nix build .#azure-devops-mcp --rebuild` and
  `nix build .#checks.x86_64-linux.azure-devops-mcp-smoke`.
- `nix build .#checks.x86_64-linux.catalog-module`.
- `nix flake check`, as CI runs it.
- **The owner's live check**, documented: connect an organization, run
  `siphon test service azure-devops`, then an agent call to `wit_work_item`.
