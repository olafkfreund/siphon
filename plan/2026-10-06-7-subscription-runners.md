---
status: approved
issue: 7
spec: spec/2026-10-06-7-subscription-runners.md
---

# Plan: Subscription-first agent runners (Claude Code, Codex, Antigravity)

## Approved decisions (self-contained)

- **Config.**
  - New top-level `credentials: {<name>: {provider: claude|codex|agy, api_key?: env:|file:, concurrency?: int (default 1)}}`.
  - Agents get `kind: claude|codex|agy` (default `claude`), `credential: <name>` and an optional `command: <binary path>`.
  - `runner:` stays as a deprecated alias for `kind: claude` with `command: runner[0]`, plus a warning; the extra args are ignored, with a warning.
  - `api_key_file:` maps to an implicit API-key credential for the agent's kind.
  - An agent with neither falls back to the subscription credential of its kind if exactly one exists; otherwise `validate` errors.
- **Store.**
  - Lives at `<dir(server.db)>/credentials/<name>/`, 0700, never in `agentgw-io`.
  - Files per provider:
    - claude: `credentials.json` (only `{"claudeAiOauth": {...}}`), or `oauth-token` (a setup-token)
    - codex: `auth.json`
    - agy: `antigravity-oauth-token`
  - Import with `agentgw credentials import -config F [--token-stdin] <name> < file`, which validates the shape. `agentgw credentials ls` prints name, provider, expiry (time only) and the last write-back time, never secrets.
- **Hand-off.**
  - `JobSpec.Files` keys may be relative sub-paths under the sandbox HOME (`jobFilesDir`).
  - `JobSpec.Writeback []string` lists paths that `exec-job` copies back if changed, as `wb-<i>` in the run dir (`O_EXCL|O_NOFOLLOW`, 0640, chowned to the run-dir gid).
  - agentgw reads them back with a 1 MiB cap.
- **Write-back** goes through a per-credential `flock` plus compare-and-swap. It saves only if the store still holds the starting bytes; otherwise it keeps the copy with the later token expiry. The new file is written to a temp name and renamed into place.
- **Concurrency:** an in-process semaphore per credential sized by `concurrency`. Workers wait and never fail.
- **Errors.** Adapter classifiers map output to:
  - `auth`: job failed with "credential <name> needs re-login: <command>", plus audit `credential_reauth`
  - `quota`: job failed with "quota/rate limit: <msg>"
  - other
- **Adapters** (`internal/action/runner_{claude,codex,agy}.go`) per the spec table:
  - claude subscription: no `--bare`, `.claude/.credentials.json`, or env `CLAUDE_CODE_OAUTH_TOKEN` for a setup-token. Keeps `--tools ""`, `--strict-mcp-config`, `--allowedTools`, `--max-turns`, `--max-budget-usd`, prompt on stdin.
  - claude api_key: `--bare` + env `ANTHROPIC_API_KEY`.
  - codex: `exec - --skip-git-repo-check --ephemeral --strict-config -s read-only -c forced_login_method=chatgpt|api`, env `CODEX_HOME=$HOME/.codex`.
    - MCP via `-c mcp_servers.<n>.command|args|url|http_headers`, plus `enabled_tools` and a per-tool `approval_mode="approve"` for each allowlisted tool.
    - API key: `.codex/auth.json` = `{"OPENAI_API_KEY": "<key>"}`.
    - Result = stdout (the final message).
  - agy: `--print=<prompt>` (one argv element), `--output-format json`, `--mode plan`, `--sandbox`, `--print-timeout <timeout>`.
    - MCP via a generated `.gemini/config/mcp_config.json` with only the listed servers.
    - API key via env `GEMINI_API_KEY` (verify live; if unsupported, `validate` errors for agy api_key).
    - Result = `response`; `status=="ERROR"` → `error` classified.
  - Result event: `{"kind", "result": <final text>, "raw": <parsed JSON or null>}`. For claude, `raw` is the full JSON and `result` its `result` field.
- **Warnings:** `validate` emits one warning per agent per control its kind can't enforce:
  - codex: built-in tools, turns, budget
  - agy: built-in tools, tool allowlist, turns, budget
- **Masking:** credential file bytes and their token fields join that run's secret mask; write-back bytes never enter the job output.
- **NixOS:** `services.agentgw.agentPackages` (list, default `[]`) goes on the template unit's `path`. The credentials dir is inside the existing StateDirectory.
- **Docs:** a README section on subscriptions and terms, the import commands, the capability table, and migration from `runner`/`api_key_file`.

## Steps

Each step is one commit; cite "Plan step N". Run `nix develop -c go test -race ./...` after each.

| # | Step | Who | Lane |
|---|---|---|---|
| 1 | exec-job: nested `Files` paths + `Writeback` | Codex | `internal/action/template.go` + test |
| 2 | Config: `credentials`, agent `kind`/`credential`/`command`, aliases, validation, warnings, schema | coder | `internal/config` |
| 3 | Credential store + `credentials import\|ls` CLI | coder | `internal/cred` (new), `cmd/agentgw` (credentials subcommands only) |
| 4 | Runner adapters + error classifier | Codex | `internal/action/runner_*.go`, `agent.go` |
| 5 | Pipeline integration: semaphore → adapter → write-back CAS → re-login audit → result event | Opus | `internal/job` |
| 6 | NixOS `agentPackages` + VM test with stub CLIs | Opus | `nix/` |
| 7 | README and docs | coder | `README.md`, `docs/`, `examples/` |
| 8 | Live checks on the owner's subscriptions | Opus | none (records results in this plan) |

1. **exec-job nested files and write-back.**
   - `JobSpec.Writeback []string`.
   - `exec-job` resolves each `Files` key with `filepath.Clean`, rejects absolute paths, `..` and empty names, creates parents 0700 with `os.MkdirAll` under `jobFilesDir`, and refuses if any parent is a symlink (`os.Lstat` walk).
   - After the child exits, for each writeback path that differs from what it wrote, it copies the file to `<runDir>/wb-<i>` (`O_EXCL|O_NOFOLLOW`, then chown to the run-dir gid, 0640).
   - `templateRun` returns the writeback bytes as `map[int][]byte`, capped at 1 MiB each.
   - Mode `none` gets the same semantics in a temp HOME dir that agentgw creates and removes.
   - **Verify:**
     - nested files land
     - `..` and symlinked parents are rejected
     - only a changed file comes back
     - the 1 MiB cap holds
   - **Trap:** keep the `RestrictSUIDSGID`-safe chown approach from #1. No setgid chmod.
2. **Config.** Implement the config decisions above, regenerate the schema, and keep `full.yaml` and all tests passing.
   - **Verify** with table tests: alias mapping, implicit api-key credential, the default-credential rule, the error when ambiguous, the per-kind warnings, `concurrency >= 1`.
3. **Credential store.**
   - `cred.Store{Dir}`:
     - `Load(name) (files map[string][]byte, err)`
     - `Save(name, path, old, new []byte) error`: compare-and-swap under `flock` on `<dir>/<name>/.lock`, temp file plus rename
     - `Expiry(provider, files) time.Time`
     - `Validate(provider, file, bytes) ([]byte, error)`: normalises the shape and strips claude `mcpOAuth`
   - `Acquire(ctx, name, n)` is the semaphore.
   - CLI `agentgw credentials import [--token-stdin] <name>` and `credentials ls`.
   - **Verify:**
     - CAS keeps the newer copy
     - stripping works
     - bad shapes are rejected
     - `ls` prints no secrets (grep the output for the token values)
     - the semaphore serialises
4. **Adapters.**
   - `type Runner interface`? No (ponytail): use `func buildRun(kind string, o AgentOptions) (argv []string, stdin []byte, env map[string]string, files map[string][]byte, writeback []string, err error)` with a switch, plus `parseResult(kind, stdout) (result string, raw any)` and `classify(kind, output) (class string)`.
   - `AgentOptions` gains `Kind`, `CredFiles map[string][]byte`, `APIKey string` and `Command string`.
   - **Verify** with stub-CLI tests per kind: exact argv/env/files, `--print=` as one element even when the prompt starts with `--`, result parsing, and that the auth/quota classifier works on recorded real messages (Codex "tool approval required" is not auth; agy `RESOURCE_EXHAUSTED` is quota).
5. **Integration.** `agentExec`:
   - resolves the credential
   - `Acquire`
   - `Load`
   - builds the run with CredFiles/APIKey and masks
   - runs the job
   - CAS-saves each writeback
   - classifies failure (on auth, audits `credential_reauth`)
   - emits the normalised agent-result
   - **Verify:**
     - a stub CLI that rotates its token updates the store
     - two concurrent agent jobs on one credential run serially
     - an auth failure gives a clear message plus an audit row
     - the agent-result `result` field is present for all kinds
6. **NixOS.**
   - Add the `agentPackages` option.
   - VM test: stub `claude`/`codex`/`agy` packages (`writeShellScriptBin`). Each checks its login file at the expected HOME path, rewrites it with a marker, and prints a result.
   - Import credentials via `agentgw credentials import`, run three agent rules, and assert:
     - each job is `done`
     - the store files carry the markers
     - the action still can't read `/var/lib/agentgw/credentials`
   - **Verify:** `nix build .#checks.x86_64-linux.vm` passes all subtests (old and new).
7. **Docs.**
   - README: subscriptions-first quick start for each provider (the login command, then import), the capability/warnings table, terms of use, API keys, and migration.
   - examples: one agent per kind.
   - **Verify:** the example validates, and README commands match the CLI.
8. **Live checks** (owner's subscriptions, on this host; `sandbox: none`, because a real sandboxed host deployment is the owner's call):
   - claude and codex agent runs through `agentgw run-once` with imported credentials and an MCP tool call
   - agy after its quota resets
   - Optional, if the owner provides them: a `claude setup-token` run, and API-key runs per provider
   - Record the results in this plan.

## Tests

- `go test -race ./...` is green after every step.
- `nix build` and `nix build .#checks.x86_64-linux.vm` pass.
- Step 8 results are recorded.

## Handoff and review

- **Codex:** steps 1 and 4 (workspace-write, no network), in the `../MCP-AgentGateway-codex` worktree on branch `feat/7-codex-lane`.
- **coder:** steps 2, 3 and 7.
- **Opus:** steps 5, 6, 8, plus integration and commits.
- Steps 1–3 run in parallel; 4 follows 1; 5 follows 2, 3 and 4; then 6 and 7; 8 is last.
- **Review:** a fresh Opus reviewer gets this plan plus the full diff before the PR. It focuses on credential handling (leaks, CAS races, symlinks) and the containment warnings.
- One PR for the whole task.

## Rollback

Revert the merge commit. The old `runner`/`api_key_file` configs keep working through the aliases, so rolling forward and back needs no config change. To remove imported logins, delete the credentials dir.

## Deviations log

- Step 2:
  - A missing credential is not an error for legacy configs (kind claude, no `credentials:` anywhere, no explicit credential), so the existing job tests keep passing. **Step 5 removes this exemption** when the pipeline uses credentials.
  - `runner` combined with a non-claude kind is an error.
- Step 3:
  - Added `Put` (an unconditional write on import, removing the provider's other login file), `Info` (expiry and mtime for `ls`) and `ReadLimited` (1 MiB import cap).
  - `Save` infers the provider from the file name.
  - Semaphore size is fixed at the first `Acquire`, so changing concurrency needs a restart.
  - Step 5 must `Validate` written-back bytes before `Save`.
- Step 7:
  - Written ahead of steps 4–5 against the approved decisions.
  - Step 8 must confirm or correct three README claims: the `claude setup-token` path, agy `GEMINI_API_KEY`, and the exact re-login message.
  - The README had no previous agent-auth section, so "Agents and subscriptions" is new.
- **Owner request mid-task: a proper devenv shell.** Added `devenv.nix`, `devenv.yaml` and `devenv.lock` (Go from devenv-nixpkgs rolling, gopls, sqlite, jq, curl, openssl, `GOTOOLCHAIN=local`), with scripts `run-tests`, `schema`, `vm-test` and `agentgw`, plus a test task hooked before `devenv:enterTest`.
  - `go.mod` relaxed from `go 1.26.8` to `go 1.26.0`, because devenv's nixpkgs has Go 1.26.7.
  - Verified: `devenv test` passes, and fails with exit 1 on a deliberately failing canary test; `nix build` is unchanged.
  - Not run: `devenv allow` (the user's consent).
- Step 5:
  - **The legacy no-credential exemption is kept**, not removed as planned. The Rollback promise ("old `runner:` configs keep working") requires it: a Claude agent with no credential uses the runner's own environment, as before.
  - The agent-result event keeps a JSON-object answer's own fields at the top level, alongside `kind`/`result`/`raw`, so existing rules (`event.ok`) still match.
  - Classification applies to failed runs only.
  - Write-back is shape-validated with `cred.Validate`, then compare-and-swap saved, then audited as `credential_refreshed`.
- Step 6:
  - The VM test has a new subtest. Stand-in `claude`/`codex`/`agy` CLIs installed via `agentPackages` run in the template-unit sandbox on imported credentials. Each finds its login at the expected HOME path, can't read `/var/lib/agentgw/credentials`, and rewrites its login; all three markers land in the store via write-back, and `credentials ls` prints no tokens.
  - **Behaviour note:** the deprecated `runner:` alias keeps only `runner[0]` (as approved), so a legacy runner that relied on extra args (the old VM `sh -c …` stand-in) must move to `command:`. The VM test was migrated.
  - VM test: 8/8.
