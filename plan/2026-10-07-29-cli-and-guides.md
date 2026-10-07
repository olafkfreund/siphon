---
status: approved
issue: 29
spec: spec/2026-10-07-29-cli-and-guides.md
---

# Plan: CLI-first management and a task-oriented user guide

## Approved decisions (self-contained)

### API (server, `internal/web`)

Every write goes through the existing `prepare`/`commit`
(`configedit.go:117,161`), so `checkOverlay` (`internal/config/overlay.go:183-277`)
and the audit apply unchanged. The CLI and MCP get no privilege the portal
lacks.

- **Item writes** (`PUT /api/config/{kind}/{name}`, `POST /api/config/apply`):
  - accept `"secrets": {"<field path>": "<value>"}`;
  - the values become `pendingSecret`s (as the portal forms do, see
    `forms.go:283-294`), written 0600 to `secrets/<kind>-<name>-<field>`;
  - the item's ref is set to that `file:`;
  - only known secret paths are allowed: `api_key`, `secret`, `auth.bearer`,
    `env.<NAME>`, `headers.<Name>`, `access_key_id`, `secret_access_key`,
    `external_id`;
  - values are never echoed, logged or returned.
- **`?dry_run=1`** on PUT, DELETE, reset and apply runs `prepare` only, and
  returns `{diff, errors, warnings}`.
- **Errors:**
  - invalid config: **422** `{"error": "<joined>", "errors": [...], "warnings": [...]}`;
  - `checkOverlay` collects every problem (the messages are unchanged);
  - `applied:false` adds `apply_error`;
  - 401, 404 and 429 are JSON too.
- **`POST /api/config/apply`:** `{"items": [{kind, name, yaml, rev?}], "delete": [{kind, name}], "secrets": {"<kind>/<name>.<field>": "…"}}`
  is one `commit` and one revision: everything applies or nothing does.
- **`X-Siphon-Actor: <label>`** (`[\w.@:-]{1,64}`) is recorded as
  `api:<label>`. The restore actor bug at `configweb.go:375` is fixed.
- **Connection and service endpoints** (JSON wrappers; add JSON tags to
  `serviceDone`, `svcTest` and `connTest`):
  - `POST /api/services/github|gitlab|aws`: returns the `serviceDone` fields,
    including the one-time hook secret, with `Cache-Control: no-store`;
  - `POST /api/services/{name}/test`;
  - `POST /api/connections/models`, and `POST /api/connections/models/{name}/test`
    (test plus the model list);
  - `GET /api/connections`: logins (status, expiry) and model endpoints;
  - `POST /api/connections/logins` `{name, provider, kind: login|token|apikey, value}`,
    and `DELETE /api/connections/logins/{name}`.
- **Diagnostics:**
  - `POST /api/rules/{name}/test` takes `{event, headers}` or
    `{use_last: true}`;
  - `GET /api/sources/{name}/last-event`;
  - `GET /api/rules/{name}/explain` returns enabled/override, the source's
    health and last error, the last event time, last fired, the cooldown
    left, the edge per-key `last_value`, whether the last event matches
    now, the last eval error, the last webhook rejection, and the recent
    fire/skip audit rows;
  - **new storage:** a `rule_error(rule PRIMARY KEY, at, error)` table,
    and `source_state.last_reject_at` / `last_reject` (status and reason
    only);
  - `/api/audit` gains the filters `rule=`, `event=` and `since=`;
  - `GET /api/config/{kind}` is sorted, with tombstones shown as
    `deleted: true`.
- **`POST /api/draft`** `{request, model?}`:
  - it uses a **model connection** (`ollama`/`openai`) to write apply-YAML;
  - the prompt holds the item schema, the matching examples, the
    inventory (names only, never secrets) and `docs/llm.md`'s rules;
  - it validates with `prepare`, feeding errors back for up to 3 rounds;
  - it returns `{yaml, diff, errors, todo, rounds, model}`;
  - **it never commits.** The model's output is untrusted, and is applied
    only through the normal apply.
  - The request text and model are kept for the audit detail when the
    draft is applied (`draft:<connection>`).

### CLI (`cmd/siphon` and a new `internal/client`)

- **Local mode** is today's behaviour, unchanged: any command given
  `-config`, and `serve`, `run-once`, `validate`, `schema` and `exec-job`.
- **Client mode:** everything else, and `jobs`/`approve`/`deny` without
  `-config`.
  - **Connection precedence:** the `-url`/`-token-file` flags, then the env
    vars `SIPHON_URL`/`SIPHON_TOKEN_FILE`/`SIPHON_TOKEN`, then
    `~/.config/siphon/client.yaml` (0600).
  - `siphon login <url>` reads the token without echo (`golang.org/x/term`;
    stdin when there's no terminal), checks it, and writes `client.yaml`.
    `siphon logout` removes it.
- **Verbs:**
  - `status`, `get <kind> [name]`, `apply -f [--dry-run] [--secret k=@file|-] [--yes]`;
  - `edit <kind> <name>` (`$EDITOR`, and `rev`/409);
  - `delete|reset <kind> <name>`, `enable|disable <rule>`, `history`,
    `restore <rev>`;
  - `new task` (wizard, or flag-only: `--source --when --on --action …`,
    with `--print` to only write the YAML);
  - `connect github|gitlab|aws|model|login`;
  - `test <rule> [file|--last]`, `test service|model <name>`, `why <rule>`;
  - `jobs [ls|show]`, `approve|deny`, `draft "<text>" [--model m] [--apply] [--yes]`.
- **LLM support:**
  - `help --json`, `explain <kind>` (from `config.Schema()`), and `example <name>`
    (embedded `docs/examples/*.yaml`);
  - `inventory -o json`;
  - `guide`, which prints the embedded `docs/llm.md`;
  - `mcp`: a stdio MCP server with the read tools `inventory`, `get`,
    `explain`, `example`, `test`, `why`, `jobs` and `status`, plus `apply`
    and `delete`, which only dry-run unless started with `--allow-write`.
    There are **no** approve or deny tools.
- **Output:** human-readable by default; `-o json` on every command;
  `--quiet`.
- **Exit codes:** 0 ok, 1 error, 2 usage, 3 validation, 4 not found,
  5 conflict.
- **Errors** go to stderr, as JSON with `-o json`:
  `{"error", "errors", "hint"}`. A `hint` names the next command to run.
- **Secrets are never taken from a plain flag value:** that's refused with
  a hint to use `-`, `@file` or the prompt. One-time secrets are printed
  once and marked.
- **Dependencies:** stdlib `flag` and `tabwriter`. The one new dependency
  is `golang.org/x/term`.

### NixOS

`services.siphon.cli.enable` (default `true`) puts the binary on PATH and
sets `SIPHON_URL` to the local listen address (`environment.variables`).

### Docs

- **The tree** (`docs/`):
  - `README.md`, `concepts.md`, `getting-started.md`;
  - `tasks/` (`webhook-command`, `poll-threshold`, `github-pr-agent`,
    `mcp-watch`, `routine`, `approvals`);
  - `connections/` (`logins`, `models`, `github`, `gitlab`, `aws`);
  - `troubleshooting.md`, `cli.md`, `configuration.md`, `llm.md`,
    `drafting.md`;
  - `examples/*.yaml`;
  - `developing/adding-a-source.md` (moved).
- **Every how-to follows one pattern:** Goal → CLI → Verify in the portal →
  YAML → Troubleshooting.
- **The README** becomes the overview plus links.
- **`llms.txt`** at the repo root.
- **CI:**
  - ` ```yaml siphon ` blocks and `docs/examples/*.yaml` are dry-run-validated
    by `docs_test.go`;
  - `cli.md` is kept in parity with `help --json`;
  - a VM subtest runs the getting-started and how-to CLI steps.

## Steps

Each step is one commit, citing "Plan step N". Go steps: `go vet ./...`,
`go test -race ./...`. Nix steps: `nix flake check`. After any `go.mod`
change, run `nix build --rebuild .#siphon.goModules`: a stale store path
once masked a wrong `vendorHash` (#24).

| # | Step | Who | Main files |
|---|---|---|---|
| 1 | API write path: secrets, dry-run, 422 + `errors[]`, `checkOverlay` collect-all, `apply_error`, actor label, restore actor, JSON 401/404/429, sorted lists with tombstones | coder | `internal/web/configapi.go`, `configedit.go`, `api.go`, `configweb.go`, `forms.go`; `internal/config/overlay.go` |
| 2 | `POST /api/config/apply` (atomic, secrets, dry-run) | coder | `internal/web/configapi.go`, `configedit.go` |
| 3 | Connection and service API endpoints | coder | `internal/web/services.go`, `models.go`, `configweb.go`, `api.go` |
| 4 | Diagnostics: rule test API, last-event, explain, `rule_error` (migration `0003`), `last_reject`, audit filters | coder | `internal/web/editors.go`, `data.go`, `api.go`; `internal/store/0003_*.sql`, `query.go`; `internal/job/queue.go`, `hooks.go`; `internal/source/webhook.go` |
| 5 | `internal/client` and CLI core: modes, `login`/`logout`, `status`, `get`, `apply`, `edit`, `delete`/`reset`, `enable`/`disable`, `history`/`restore`, `jobs`/`approve` in client mode, `-o json`, exit codes, hints, `help --json` | coder | `internal/client/`, `cmd/siphon/` |
| 6 | CLI: `connect`, `test`, `why`, `new task` (wizard and flags) | coder | `cmd/siphon/` |
| 7 | LLM self-description: `explain`, `example`, `inventory`, `guide`; `docs/examples/*.yaml` and the first `docs/llm.md` | coder (Go) + Opus (`examples`, `llm.md`) | `cmd/siphon/`, `docs/examples/`, `docs/llm.md` |
| 8 | `siphon mcp` | coder | `cmd/siphon/mcp.go` |
| 9 | `siphon draft` and `POST /api/draft` | coder | `internal/draft/`, `internal/web/api.go`, `cmd/siphon/` |
| 10 | NixOS `cli.enable`, plus a VM subtest of the CLI walkthrough | Opus | `nix/module.nix`, `nix/vm-test.nix`, `flake.nix` |
| 11 | The docs tree, README trim, `llms.txt`, `docs_test.go` (YAML blocks, examples, `cli.md` parity) | Opus | `docs/`, `README.md`, `llms.txt`, `docs/docs_test.go` |
| 12 | Review (fresh Opus), owner live, PR | Opus | none |

1. **API write path.**
   - **Secrets:** PUT bodies decode `secrets`; the paths are checked
     against an allowlist and turned into `pending` for `commit`. Reuse
     `forms.go`'s `pendingSecret` and file naming; don't duplicate it.
   - **`?dry_run=1`:** call `prepare`, and return the diff (the portal's
     `/check` already builds one, see `configweb.go:262-291`), the errors
     and the warnings.
   - **422:** split `Validate`'s joined error into `errors`, and make
     `checkOverlay` append instead of returning at the first failure,
     keeping the messages identical.
   - **The rest:** `apply_error` comes from the live-apply error
     (`configedit.go:256-263`); the actor header; the `configweb.go:375`
     actor; JSON from `api.go:107,114`; listing sorted with tombstones.
   - **Tests:** every point, including "dry-run commits nothing", "secret
     file 0600 and never in a response", and "unknown secret path → 422".
   - **Traps:**
     - the existing overlay tests must pass with the same messages;
     - the portal's routes and behaviour must not change.
2. **Atomic apply.**
   - `POST /api/config/apply`: build all the items and deletes into one
     `putItems`-style mutation and one `commit` (see how `addAWS` commits
     several items).
   - Secrets are keyed `<kind>/<name>.<field>`. Supports `dry_run`.
   - **Tests:** a bad item means nothing is applied; one revision; deletes
     included; secrets; dry-run.
3. **Connection and service endpoints.**
   - Wrap `addGitHub`, `addGitLab`, `addAWS`, `testService`, `addModelConn`,
     `listModels`, `addLogin` and `removeLogin`. Each already takes an
     `actor`; pass the API actor label.
   - Add JSON tags (snake_case) to `serviceDone`, `svcTest` and `connTest`.
   - `GET /api/connections` merges the credentials list, the login status
     (as `credentials ls` builds it) and the model endpoints.
   - **Tests:**
     - each endpoint's success and refusal;
     - the one-time secret is returned once, with `no-store`;
     - nothing secret appears in `GET`;
     - the portal pages are unchanged.
4. **Diagnostics.**
   - **`runRuleTest`** (`editors.go:140`): split it into
     `ruleTest(cfg, rule, event, headers) testView` plus the form adapter,
     used by both routes.
   - **The new routes:** last-event (redacted `source_state.json`, see
     `overlay.go:145-175`) and `explain`.
   - **Migration `internal/store/0003_diagnostics.sql`:** the `rule_error`
     table and the `source_state` columns. Write the eval errors
     (`queue.go:125`, `hooks.go:57`) and the webhook rejections
     (`webhook.go` reject paths: status and reason only, never the body or
     the signature).
   - **Audit filters.**
   - **Tests:** every `explain` reason (disabled, cooldown, edge already
     true, eval error, rejected webhook, no events yet).
   - **Trap:** the webhook handler is rebuilt per request, so record
     rejections through a callback or the store, not in handler state.
5. **Client and CLI core.**
   - **`internal/client`:** the config precedence, 0600 writes, JSON calls
     with the bearer token and `X-Siphon-Actor: cli:$USER`, and error
     decoding into a typed error with the exit code.
   - **`cmd/siphon`:** a dispatch table with the mode rule, and every verb
     in this step.
   - `help --json` is generated from the same table that prints usage
     (the single source of truth).
   - **Tests:** each verb against an `httptest` server built from
     `internal/web` with a temp store (a real API, not a mock), plus
     precedence, 0600, the exit codes, and that local mode is unchanged
     (the existing `cmd/siphon` tests).
   - **Trap:** keep `-config` meaning local mode everywhere. Never send
     the token in a URL or log it.
6. **Guided commands.**
   - **`connect`:**
     - prompts, or flags for non-secrets;
     - secrets only from `-`, `@file` or the no-echo prompt;
     - a plain value is refused (exit 2, with a hint);
     - one-time secrets printed once.
   - **`test`, `why`:** `why` renders `explain` as a checklist with the
     likely reason last.
   - **`new task`:** the wizard and flag-only forms both produce the same
     apply-YAML. Then a dry-run, a confirmation (`--yes`), and the apply.
   - **Tests:** the flag-only paths, the refused plain secret, and the
     wizard driven through a scripted stdin.
7. **Self-description.**
   - `explain <kind>`: fields, types, required fields, defaults and enums
     from `config.Schema()`.
   - `example <name>`: `//go:embed` the files in `docs/examples/`.
   - `inventory`: one call or several (names, providers, `mcp_packages`,
     `server.aws` lists, private endpoints). Never secrets.
   - `guide`: embeds `docs/llm.md`.
   - **Opus writes `docs/examples/*.yaml` and the first `docs/llm.md`
     before the coder starts this step.**
   - **Tests:** every example dry-runs OK; `explain` covers every kind;
     `inventory` has no secret values (assert against the test
     credentials).
8. **`siphon mcp`.**
   - go-sdk server over stdio; its tools call the `internal/client`
     functions.
   - `apply`/`delete` force `dry_run` unless `--allow-write`. No
     approve or deny tools.
   - **Tests:** a go-sdk client over in-memory transports lists the tools;
     a write without the flag is a dry-run only; there's no approve tool.
9. **Draft.**
   - **`internal/draft`:** builds the prompt (schema, examples matched by
     keywords from the request, inventory, the rules from
     `docs/llm.md`), makes one OpenAI-compatible chat completion against
     the model connection's URL and key, and extracts the YAML.
     - Reuse the chat request and response shapes from `internal/agentloop`,
       or a minimal copy if exporting them is messy.
     - The model call honours the same private-endpoint guard as
       `listModels`.
   - **Repair loop:** run `prepare` through apply's dry-run, up to 3
     rounds.
   - **`POST /api/draft`**, and the CLI `draft` (prints; `--apply`
     confirms or takes `--yes`; the audit detail `draft:<model>`).
   - **Tests:** with the `nix/stub-model.py` approach, or an `httptest`
     model: bad YAML, then a repair with the errors, then success; secrets
     never in the prompt (assert the captured request); no commit without
     apply.
10. **NixOS.**
    - `cli.enable` puts `environment.systemPackages` and
      `environment.variables.SIPHON_URL` in place (from the listen
      setting; `http://127.0.0.1:<port>`).
    - An eval check.
    - **VM subtest:** as a normal user,
      1. `siphon login` (token on stdin);
      2. `connect model` (the stub);
      3. `apply -f` an example;
      4. `new task --print | apply -f -`;
      5. `test`;
      6. fire the webhook;
      7. `why`;
      8. `jobs`;
      9. `approve`;
      10. `draft` against the stub model;
      11. `mcp` lists its tools.

      Assert the documented key lines.
    - **Trap:** never `pkill -f` a pattern that's in the calling shell's
      command line.
11. **Docs.**
    - The full tree, written from the finished CLI.
    - `docs/developing/adding-a-source.md` is moved.
    - The README trimmed to an overview, plus `llms.txt`.
    - **`docs_test.go`:**
      - extracts the ` ```yaml siphon ` blocks and dry-run-validates them
        against a base config;
      - validates `docs/examples/*.yaml`;
      - checks `cli.md` against `help --json` both ways;
      - checks relative links resolve.
12. **Review and PR.**
    - A fresh Opus review against this plan and `git diff`: secrets, the
      no-new-privilege claim, the MCP write gate, draft injection and
      untrusted output, token handling.
    - Owner live on p620:
      - getting-started from a clean shell;
      - one real task from the CLI, seen in the portal;
      - `siphon draft` with the local Ollama;
      - Claude Code driving `siphon mcp`.
    - Then the PR.

## Tests

- `go test -race ./...`, `devenv test`.
- `nix flake check`: the VM test with the CLI walkthrough subtest, plus the
  eval check.
- The docs tests.
- CI green.
- The owner's live checks (step 12).

## Handoff

- **Coder:** steps 1–9 (Go), one agent, steps sent in order with
  SendMessage.
- **Opus:**
  - steps 10–12;
  - `docs/examples/*.yaml` and the first `docs/llm.md` before step 7;
  - docs drafting can start alongside steps 5–9.

## Rollback

Revert the merge.
- **Server:** the additions are additive; the existing routes and the
  portal are unchanged.
- **Migration `0003`:** only adds a table and columns.
- **CLI:** local mode is unchanged, and client mode is new.

## Deviations log
- **Step 1 (coder):**
  - Invalid config returns 422, including under `?dry_run=1` (same body as a real write); the existing tests' 400 assertions were updated.
  - `external_id` is not a secret path: it's a plain string, not a ref.
  - `warnings` is always `[]` until something produces warnings.
  - Unknown `/api/…` paths return JSON 404 (the catch-all route).
  - Helpers for later steps: `s.write`, `withSecrets`, `secretPath`, `apiActor`, `s.dryRun`, `errInvalid.list()`.
  - **Gap fixed in step 2:** dry-run skipped the model-endpoint private-address check, which lived in `commit`.
- **Step 2 (coder):**
  - The model-endpoint check lives in `credsCheck`, shared by `commit` and `dryRun`.
  - In a batch, every item's `rev` must equal the latest revision (mixed revs give 409).
  - Errors that name an item get the `<kind>/<name>:` prefix. `checkOverlay` messages don't name one yet; that's fixed in step 3.
  - The revision summary is `apply: N items, M deleted`.
