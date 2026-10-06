---
status: draft
issue: 7
author: olafkfreund
---

# Intent: Subscription-first agent runners (Claude Code, Codex, Antigravity)

Follows `intent/2026-10-06-1-agent-gateway.md` (approved, implemented).

## Problem

agentgw's agent action can only run Claude Code, and inside the sandbox it can
only authenticate with an Anthropic API key. That is expensive, and most
people who would use agentgw already pay for a subscription instead:

- **Claude Code.** The default runner `claude --bare -p` reads only
  `ANTHROPIC_API_KEY` or `apiKeyHelper`; `--bare` never uses a subscription
  login. A subscription run works today only outside the sandbox, as
  `[claude, -p]` with `sandbox: none` on a workstation (verified 2026-10-06).
- **Codex (OpenAI).** It is not supported as a runner. Its ChatGPT-plan login
  lives in `$CODEX_HOME/auth.json` (id, access and refresh tokens).
- **Antigravity (`agy`, Google).** It is not supported as a runner. Its Google
  login lives in `~/.gemini/oauth_creds.json`.

The sandboxed agent unit has an empty HOME, so none of these logins reach it.
Subscription OAuth tokens also refresh and can rotate. A throwaway sandbox
that refreshes a token and then discards it can leave the stored credential
stale.

The owner states this is a **hard requirement**: subscription use makes
agentgw accessible to many more users. API keys must keep working.

## Proposed outcome

An operator can:

1. Pick the runner per agent: `claude`, `codex` or `agy`.
2. Pick auth per agent: **`subscription`** (the default) or **`api_key`**.
3. Log in once on the host with each tool's normal login flow, then point
   agentgw at the result (the `claude setup-token` token, the Codex
   `auth.json`, the agy OAuth credentials). Agents then run on that
   subscription **inside the systemd sandbox**, in the NixOS module, not only
   on a workstation.
4. Keep subscriptions working over time. Token refreshes made during a run
   are saved back, so the login doesn't silently expire or get invalidated
   by token rotation. When re-login is needed, the operator sees a clear
   error in the job output and audit log.
5. Keep the same safety contract for every runner, as far as each CLI
   allows:
   - the prompt goes on stdin, never argv
   - an exact tool allowlist, with built-in shell/file tools off
   - only the agent's listed MCP sources
   - turn, time and budget limits
   - approval on by default
   - credentials never in argv, logs or readable paths
   Where a CLI can't enforce one of these, the docs say so and `validate`
   warns.
6. Check it all: unit tests per runner using stubs, the NixOS VM test with
   stub CLIs, and one manual live run per provider on a real subscription.

## Affected users and systems

- Every agentgw user. Most have subscriptions rather than API keys.
- `internal/action` (the agent runner and job spec), `internal/config`
  (agent `runner`/`auth`), `nix/module.nix` (credential storage for the
  template unit), the VM test, and the README.
- The owner's Claude, ChatGPT and Google accounts, for the manual checks.

## Constraints

Must:

- Make subscription auth first-class and the default. API keys stay
  supported for all three.
- Run inside the existing `agentgw-action@` sandbox. No weakening:
  - no transient units
  - PID 1 still opens nothing in agentgw-writable paths
  - credentials reach a run only through the job hand-off or another path
    that only agentgw and that run's sandbox user can read
- Never put credentials into argv, the Nix store, logs, job output or the
  audit log. Mask them like every other secret.
- Handle token refresh without losing the login, including two agent runs at
  once against the same login.
- Keep the provider-specific code small: one runner adapter per CLI, chosen by
  config. No plugin system.
- Stay honest about each provider's terms of use for automated or headless use
  of consumer subscriptions, and document them.

Must not:

- Weaken the containment that #1 established for Claude (prompt on stdin,
  `--tools ""`, `--strict-mcp-config`, exact allowlist).
- Require running the gateway or actions as a real login user with a home
  directory.

## Open questions

1. **Terms of use.** Running consumer subscriptions from an unattended server
   may be limited by each provider's terms. Should agentgw document the terms
   and leave compliance to the operator, or refuse subscription mode unless
   the operator explicitly acknowledges them in config?
2. **Refresh model.** Should agentgw own a credential store per login that
   runs read from and write refreshed tokens back to (serialising refreshes
   across concurrent runs)? Or rely on long-lived tokens where a provider has
   them (Claude `setup-token`) and accept re-login for the others? The spec
   would evaluate both.
3. **Containment gaps.** Codex and agy may not support every control (an exact
   tool allowlist, MCP scoping, turn/budget limits) the way Claude does. If a
   control can't be enforced for a runner, should `validate` refuse that agent
   config, or warn and allow it?
4. **Scope of v1.** All three runners at once, or Claude first (the smallest
   step, since a long-lived subscription token exists), then Codex, then agy?
