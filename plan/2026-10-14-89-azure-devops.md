---
status: approved
issue: 89
spec: spec/2026-10-14-89-azure-devops.md
---

# Plan: Azure DevOps service hooks trigger rules

## Approved decisions

- **No new Go code in `internal/source` or `internal/config`.** The
  existing `token` mode verifies `X-Siphon-Key`, which the Web Hooks
  "HTTP headers" field sends.
- **The replay key** is the body hash, as for every `token` source. There's
  no `body.<path>` id: ids are unsigned, and Azure DevOps retries are
  byte-identical.
- **Catalog `azure-devops`:**
  - it creates `<name>-hooks` (`hook: true`, `token`, `X-Siphon-Key`,
    `{{generated "secret"}}`);
  - the setup text says to add the header, leave basic auth empty, choose
    "Resource details: All", and set `public_url`;
  - `templates: [azure-devops-build-failed, azure-devops-new-bug]`.
- **Templates:**
  - `azure-devops-build-failed` fires on `build.complete` with result
    `failed`;
  - `azure-devops-new-bug` fires on `workitem.created` with
    `System.WorkItemType == "Bug"`;
  - both use `on: each`, `id: string(event.resource.id)` and ntfy.
- **Agent tools** are a follow-up issue.

## Steps

1. **Catalog** (`internal/catalog/services.yaml`, `catalog_test.go`).
   - Replace the not-yet line (`:1012`) with the spec's entry.
   - Add `TestAzureDevOpsEntry`: it renders `<name>-hooks` with
     `signature: token`, `token_header: X-Siphon-Key` and a generated
     secret (in `Secrets`, not in the YAML), and `Done.Hook` is set.
   - **Verify:** `go test ./internal/catalog ./internal/web`
     (`TestCatalogEntriesRenderAndCommit` and
     `TestNotYetServicesCannotConnect`, which still needs at least one
     not-yet entry; Kubernetes stays).

2. **Templates** (`docs/templates/azure-devops-build-failed.yaml`,
   `azure-devops-new-bug.yaml`, `docs/templates/README.md`,
   `cmd/siphon/describe_test.go`).
   - The two templates per the spec, with the header `needs:` lines and
     the ntfy argument starting with literal text.
   - **Check:** that expr-lang reads
     `event.resource.fields["System.WorkItemType"]`. If
     `TestTemplatesValidate` refuses it, use the form that works and log
     it.
   - **`baseConfig`** gets the `azure-devops-hooks` line from the spec.
   - **Verify:** `go test ./docs ./cmd/siphon`.

3. **Docs.**
   - The webhook docs (`docs/tasks/webhook-command.md`): an Azure DevOps
     line under `token` (the HTTP headers field).
   - `docs/llm.md`: one line.
   - Then run `go generate ./docs`.
   - **Verify:** `go test ./docs`.
   - **Traps:** no hand-edited generated files.

4. **All checks, curl, and the follow-up issue (mine).**
   - `go vet ./... && go test -race ./...`.
   - Curl Microsoft's sample `build.complete` payload (result `failed`)
     with the header to a throwaway Siphon: the rule fires, and a wrong
     header gets 401.
   - Open the "Azure DevOps agent tools" follow-up issue.

## Tests

```sh
go test ./internal/catalog ./internal/web ./docs ./cmd/siphon
go vet ./... && go test -race ./...
```

## Rollback

Revert the PR. Existing `token` sources are unaffected.

## Implementation

Steps 1–3 edit about 8 files, so a `coder` agent does them and a fresh
Opus agent reviews them. Step 4 is mine.

## Deviations

- **Step 2:** the new-bug notification reads the title with
  `{{index .event.resource.fields "System.Title"}}`, because a dotted key
  can't be read as `.field`. expr-lang accepts
  `event.resource.fields["System.WorkItemType"]` in `when` as written.
- **Step 3:** `docs/llm.md` names the build template only, to keep it to
  one line.
