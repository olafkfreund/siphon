---
status: approved
issue: 12
author: olafkfreund
---

# Intent: Built-in egress restriction for sandboxed agents and actions

## Problem

Sandboxed runs (`agentgw-action@` units) can open network connections to
**any host on the internet**. Only cloud-metadata addresses are blocked. The
README currently admits this as a known risk:

- **Subscription logins can be stolen.** A subscription agent has a copy of
  its login (including the refresh token) in its private HOME. A
  prompt-injected agent, steered by hostile data from a webhook, an MCP
  source or a tool result, can send that token anywhere. The attacker then
  holds a long-lived login to the owner's Claude, ChatGPT or Google account.
- **Data can leak.** The same holds for anything else an agent reads: MCP
  tool results, event payloads, files in its workspace.
- **Commands are equally open.** Plain `cmd` actions can also reach anything.
- **The current mitigation is manual.** Today it means "use a dedicated
  account and set up your own proxy or `IPAddressAllow` override". In
  practice few operators will, and the subscription feature makes the token
  far more valuable than an API key.

## Proposed outcome

1. **Default deny for agents.** Sandboxed agent runs can only reach the hosts
   they need: their provider's API endpoints, the MCP servers the agent is
   configured to use, and hosts the operator explicitly allows. Everything
   else is refused, and the refusal is logged and audited.
2. **No setup for the common case.** The provider endpoints for claude, codex
   and agy ship with agentgw. An operator who changes nothing gets the
   restriction. Turning it off, or adding hosts, is one config setting per
   agent (and globally).
3. **Hard for an agent to evade.** An agent that ignores proxy settings, or
   tries raw IP connections or DNS tricks, still cannot reach other hosts.
   The restriction is enforced by the sandbox around the run, not by the
   agent's cooperation.
4. **The same option for `cmd` actions.** Either opt-in or default-on; see
   open question 2.
5. **Visible.** `validate` shows each agent's effective allowlist. Blocked
   attempts appear in the job output and the audit log with the host
   requested, so an operator can tell a legitimate missing entry from an
   attack.
6. **Tested.** The NixOS VM test proves an agent can reach an allowed host and
   cannot reach a blocked one, including by raw IP.

## Affected users and systems

- Every operator running agents in the systemd sandbox (the NixOS module).
- `internal/action` (the sandbox and runner env), `internal/config`
  (allowlists), the NixOS module (unit network settings), possibly a small
  egress component inside agentgw, the VM test, and the README.

## Constraints

Must:

- **Enforce in the sandbox, not by trusting the agent.** An environment
  variable the agent can ignore is not enough on its own.
- **Work for all three CLIs on subscriptions.** Provider endpoints sit behind
  CDNs and change IPs, so a static IP allowlist alone won't work. Host-name
  (TLS SNI / CONNECT host) filtering is the likely shape.
- **No TLS interception.** The agent's provider traffic stays end-to-end
  encrypted; agentgw must not hold or present certificates for it.
- **Stay small.** No new daemon to operate if agentgw can do it itself, and
  stdlib only where possible, consistent with the "one binary" design.
- **Keep the existing security properties.** No transient units, PID 1 opens
  nothing agentgw can write, credentials stay out of argv and logs.
- **`sandbox: none` can't enforce this.** The docs and `validate` must say so
  plainly.

Must not:

- Break local stdio MCP servers, which use no network.
- Silently break existing deployments without a clear `validate` message
  about what to allow.

## Open questions

1. **Default.** On by default for agents (safer, and may need allowlist
   additions after upgrading), or off by default with a strong `validate`
   warning? My recommendation: **on for agents**, with the provider
   endpoints built in, so most setups need no change.
2. **`cmd` actions.** Restrict them too, by default or opt-in? They often
   reach arbitrary hosts by design (deploy hooks, notifications).
   Recommendation: **opt-in per rule or globally**, with the same mechanism.
3. **Where the provider host lists live.** Hard-coded in agentgw and updated
   with releases (simple, but a provider adding a host needs an agentgw
   update), or a shipped default list operators can override in config?
   Recommendation: **built-in defaults, plus per-agent `egress.allow`
   additions**, and a global override for emergencies.
4. **Granularity.** One allowlist per agent (precise; needs a way to tell
   runs apart inside the proxy), or one shared list for all sandboxed runs
   (simpler, looser)? Recommendation: **per agent**.

## Resolutions (approved 2026-10-06, recommendations)

1. On by default for agents, with built-in provider endpoints.
2. `cmd` actions: opt-in, per rule or globally.
3. Built-in provider host defaults, plus per-agent `egress.allow`, plus a global override.
4. Per-agent allowlists.
