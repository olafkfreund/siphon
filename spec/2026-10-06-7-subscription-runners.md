---
status: draft
issue: 7
intent: intent/2026-10-06-7-subscription-runners.md
---

# Spec: Subscription-first agent runners (Claude Code, Codex, Antigravity)

## Evidence (live checks on this host, 2026-10-06)

Each CLI ran with **only a copy of its login file** in a throwaway, empty HOME
under `env -i` (no API keys present). The copies were deleted after each run.

| CLI | Login file | Result |
|---|---|---|
| Claude Code 2.x | `~/.claude/.credentials.json` → only the `claudeAiOauth` object (access, refresh, expiresAt) | `claude -p --tools "" --strict-mcp-config … --output-format json` answered "OK" on the **Max subscription**. (`--bare` never reads OAuth; that is why subscription runs drop it.) |
| Codex 0.159 | `$CODEX_HOME/auth.json` (id, access, refresh tokens, account id) | `codex exec --ephemeral --strict-config -s read-only -c forced_login_method=chatgpt` ran on the **ChatGPT plan** (model `gpt-6.1-sol`). With `-c mcp_servers.demo.enabled_tools=["get_status"]` and `-c mcp_servers.demo.tools.get_status.approval_mode="approve"`, it called the demo MCP tool and answered "degraded". Without the approval key, the call is blocked ("tool approval required"). |
| Antigravity `agy` 1.2.16 | `~/.gemini/antigravity-cli/antigravity-oauth-token` (`token.{access_token,refresh_token,expiry}`, `auth_method`) | Authenticated from the copy alone: Google answered with the account's own `RESOURCE_EXHAUSTED: Individual quota reached… Resets in 9h35m`. agy created `~/.gemini/config/mcp_config.json`, the file that holds its MCP servers. The stale `~/.gemini/oauth_creds.json` is not used. |

Token lifetimes seen: the Claude access token lasts hours, the Codex access
token about 10 days, and the agy access token is refreshed on every use.
Codex and Claude refresh tokens are assumed to rotate; Google's normally do
not.

## Design

### Config

```yaml
credentials:                      # logins agentgw owns (store: <state>/credentials/<name>/)
  claude-max:  { provider: claude }
  chatgpt:     { provider: codex }
  google:      { provider: agy }
  openai-key:  { provider: codex, api_key: file:/run/credentials/agentgw.service/openai }

agents:
  triage:
    kind: codex                   # claude (default) | codex | agy
    credential: chatgpt           # subscription login from `credentials:`
    prompt: "…"
    mcp: [factory]
    allowed_tools: [mcp__factory__task_status]
    max_turns: 10                 # enforced where the CLI can (see table)
    max_budget_usd: 1
    timeout: 10m
```

- `kind` selects a runner adapter. `command:` optionally overrides the binary
  path. It replaces the free-form `runner` argv, which stays accepted as a
  deprecated alias for `kind: claude` with a `validate` warning.
- `credential` names an entry in `credentials`. An entry is a subscription
  login (the store files) or, with `api_key: env:|file:`, an API key.
  **Subscription is the default, API keys stay supported** (intent
  resolution 5).
- The old `api_key_file` maps to an implicit API-key credential, kept for
  compatibility.

### Credential store and import

- Location: `<dir of server.db>/credentials/<name>/`, mode 0700, owned by
  agentgw. Under the module that is `/var/lib/agentgw/credentials`, which is
  never shared with the sandbox (it isn't in `agentgw-io`).
- Import with `agentgw credentials import -config F <name> < loginfile`. This
  reads stdin, so it works across users:
  `sudo -u agentgw agentgw credentials import … chatgpt < ~/.codex/auth.json`.
  - It validates the shape per provider and stores only what is needed. For
    Claude that is just the `claudeAiOauth` object; MCP OAuth entries are
    dropped.
- Claude also accepts a `claude setup-token` token (`--token-stdin`), used as
  `CLAUDE_CODE_OAUTH_TOKEN`. That avoids refresh entirely. Before the docs
  recommend it, a live check must confirm the variable is read in non-bare
  mode.
- `agentgw credentials ls` shows names, providers, the token expiry
  (time only) and last write-back. Secrets are never printed.

### Hand-off into the sandbox (no new trust paths)

- Credential files travel in `JobSpec.Files` like the MCP config and API key
  do today. `Files` gains **relative sub-paths** under the sandbox HOME
  (`jobFilesDir`, `/tmp/agentgw`):
  - `.claude/.credentials.json`
  - `.codex/auth.json` (with `CODEX_HOME=$HOME/.codex`)
  - `.gemini/antigravity-cli/antigravity-oauth-token`
  - `exec-job` rejects absolute paths, `..`, and symlinked parents. Each file
    is written 0600 into the unit's PrivateTmp.
- **Write-back.** The JobSpec lists `writeback` paths. After the child exits,
  `exec-job` copies each one, if it changed, into the run directory as
  `wb-<i>` (`O_EXCL|O_NOFOLLOW`, group-readable, the same mechanism as
  stdout). agentgw reads it back with a cap and validates the
  provider-specific shape.
  - It saves atomically (temp file plus rename) under a per-credential
    `flock`, **only if** the store still holds the bytes this run started
    with (compare-and-swap). Otherwise it keeps the newer of the two by token
    expiry.
- **Concurrency.** `credentials.<name>.concurrency`, default **1**, limits
  concurrent runs per login. The worker waits for a slot and doesn't fail.
  With rotating refresh tokens this keeps two runs from racing a refresh.
  Subscriptions are rate-limited per account anyway.
- **Re-login detection.** Each adapter matches its CLI's auth-failure output
  (for example `invalid_grant`, `Not logged in`, HTTP 401, `login`). On a
  match the job fails with "credential <name> needs re-login: run …" and an
  audit row `credential_reauth`. Quota errors (`429`, `RESOURCE_EXHAUSTED`,
  "quota") are reported as such and are not treated as auth errors.

### Runner adapters (one file each, `internal/action/runner_*.go`)

Each adapter builds argv, env, files and writeback for its CLI, and parses
the result into a common shape:
`{"kind", "result": <final text>, "raw": <the CLI's JSON if any>}`. That
shape becomes the `agent-result` event data. Claude's JSON already has
`result`, so it is extended rather than broken.

| Control | claude | codex | agy |
|---|---|---|---|
| Prompt | stdin (`-p`) | stdin (`exec -`) | `--print=<prompt>`, a single argv element. agy uses Go's `flag` parser, so the value can't be read as a flag. This is a documented deviation from stdin, checked in a test. |
| Built-in tools off | `--tools ""` | ✗ shell can't be disabled → `-s read-only` (Codex's own sandbox) **warn** | ✗ → `--mode plan` + `--sandbox` **warn** |
| Exact MCP tool allowlist | `--allowedTools` + `--strict-mcp-config` | `enabled_tools` + per-tool `approval_mode="approve"`, config from `-c` only (HOME has none) | ✗ only the listed MCP servers in a generated `mcp_config.json` **warn** |
| Turns / budget | `--max-turns`, `--max-budget-usd` | ✗ **warn**; timeout only | ✗ **warn**; `--print-timeout` + timeout |
| Subscription auth | `.claude/.credentials.json` (non-bare) or `CLAUDE_CODE_OAUTH_TOKEN` | `.codex/auth.json`, `forced_login_method=chatgpt` | `.gemini/antigravity-cli/antigravity-oauth-token` |
| API key | `--bare` + `ANTHROPIC_API_KEY` env | `.codex/auth.json` = `{"OPENAI_API_KEY": …}`, `forced_login_method=api` | `GEMINI_API_KEY` env, if agy honours it (verify; otherwise unsupported and `validate` errors) |
| Output | `--output-format json` → `result` | stdout = final message | `--output-format json` → `response` |

- `validate` emits one warning per agent per unenforceable control, naming
  it. Nothing is refused (intent resolution 3).
- Claude subscription runs drop `--bare`. Inside the sandbox HOME holds only
  our files, so there are no user hooks or settings to load. That is
  documented as the reason this is safe in the sandbox and not on a
  workstation.

### NixOS module

- No new privileges are needed: credentials stay in agentgw's private
  StateDirectory and travel through the existing job hand-off.
- The template's `path` must include the CLIs. The module adds a
  `services.agentgw.agentPackages` option (default: none; for example
  `[ pkgs.claude-code pkgs.codex ]`).
- `HOME=/tmp/agentgw` inside the unit (PrivateTmp) is already there.

### Terms of use

README section "Subscriptions and terms": links to each provider's terms, a
plain statement that automated or headless use of a consumer subscription
may be restricted, and that compliance is the operator's (intent resolution 1).

## Alternatives rejected

- **Mount or bind the user's real login directory into the sandbox.** That
  would give a prompt-injected agent the user's whole HOME, and PID 1 would
  resolve the paths (the class of bug fixed in #1's C1).
- **Run agents as a real login user with a home directory.** The intent
  rules this out.
- **API keys only.** The owner's hard requirement is subscriptions.
- **Unlimited concurrency per login with optimistic write-back.** Rotating
  refresh tokens make two simultaneous refreshes invalidate each other; a
  default of 1 is simple and safe, and operators can raise it.
- **A plugin system for runners.** Three adapters, chosen by a switch.

## Risks

| Risk | Mitigation |
|---|---|
| A provider changes its login-file format or headless behaviour | Shape validation at import and write-back; a clear re-login error; live checks per release noted in the README |
| Refresh-token rotation loses the login | Write-back with compare-and-swap, concurrency 1 per credential, Claude setup-token as a non-rotating option |
| Codex/agy containment is weaker than Claude's | Per-control `validate` warnings, documentation, and the unchanged systemd sandbox as the outer boundary |
| agy prompt in argv | It's a single `--print=` element, parsed by Go's `flag`, and invisible to other users (`ProtectProc=invisible`) |
| Subscription terms | README statement; the operator decides |
| A credential file leaks into job output | Credential bytes are added to the secret mask for that run; write-back files are never copied into the job output |

## Verification

- **Unit tests per adapter**, using stub CLIs: exact argv, env and files;
  stdin vs `--print=`; and parsing of the result shape.
- **Write-back:**
  - A stub CLI rewrites its token file, and the store updates under
    compare-and-swap.
  - A stale run doesn't overwrite a newer store.
  - Concurrency 1 serialises runs.
  - Auth-failure and quota outputs map to the right errors.
- **Hand-off security:** `exec-job` rejects `..`, absolute paths and symlinked
  parents in file paths; credential bytes are masked in output.
- **`validate`:** the warnings table holds for each kind.
- **NixOS VM test:** a stub `claude`, `codex` and `agy` installed via
  `agentPackages` each run in the sandbox. Each asserts its login file is at
  the expected HOME path and rewrites it. The test checks the store got the
  rewrite and the action still can't read `/var/lib/agentgw`.
- **Live, manual, on the owner's subscriptions:** one agent run each through
  `agentgw run-once` in the sandbox, with an MCP tool call where supported.
  agy waits until its quota resets. Also one API-key run per provider where a
  key is available.
