---
status: approved
issue: 1
author: olafkfreund
---

# Intent: Self-hosted MCP/API agent gateway

## Problem

Agents today only act when someone talks to them. There is no small,
self-hosted way to say "watch this MCP server, API or webhook, and when the
data looks like X, run this agent, routine or command".

What exists does not fit (research 2026-10-06, cross-checked by Codex):

- MCP gateways (agentgateway, IBM ContextForge, Docker/Microsoft MCP Gateway,
  MetaMCP, Obot) proxy, authenticate and audit tool calls. None of them
  watches upstreams, evaluates rules or triggers actions.
- Automation engines (Windmill, StackStorm, n8n, Node-RED) do
  trigger → condition → action. They use MCP only as agent tools, not as an
  event source. They are heavy (Postgres, or RabbitMQ+Mongo) and keep logic in
  a canvas or database rather than reviewable config.
- MCP itself has no standard push mechanism yet. The 2026-07-28 spec replaced
  resource subscriptions with `subscriptions/listen` (a "changed, refetch"
  hint). The Triggers & Events working group is still early.

The owner has decided to build this as our own platform, not adopt or extend
Windmill or StackStorm. Windmill is used as the main **reference design**: we
study its code and may reuse parts where the licence allows. It also ships a
`flake.nix` and a NixOS module (`services.windmill`), which fits our Nix
packaging.

### Windmill as prior art (checked 2026-10-06, `windmill-labs/windmill`)

Licence split, from its `LICENSE`:

| Part | Licence | What we may do |
|---|---|---|
| `backend/` (Rust), `frontend/` (Svelte) | AGPLv3 | Read and learn freely. Copying code makes our project AGPLv3, including the network-use clause. |
| Code behind the `enterprise` compile flag or a licence check | Proprietary | **Never copy.** Forks "MUST not include" it. |
| `openflow.openapi.yaml` (OpenFlow spec), `python-client/`, `go-client/`, other clients | Apache 2.0 | Reuse freely, with attribution. |

Owner direction: **review and learn from the code, keep ours easier to
manage and develop.** A structured code review of these areas is part of the
spec stage. Areas worth studying: trigger model (schedules, webhooks, polling), job
queue and worker claim/lease, approval steps, flow/routine format (OpenFlow),
AI agent steps with MCP tools, CLI sync of config to git, and Nix packaging.

Differences to keep in mind: Windmill's queue is built on Postgres, and the
whole system is a full multi-tenant platform. Our footprint constraint below
rules out adopting that wholesale.

## Proposed outcome

An operator can:

1. Declare sources in one config file: MCP servers (resource or declared
   read-only tool polled on a schedule, with change hints where supported),
   REST APIs polled on a schedule, and inbound webhooks.
2. Declare rules: a condition over the incoming data, with explicit firing
   semantics (fire when the condition becomes true, or once per upstream
   event), and a cooldown.
3. Bind each rule to an action: run an AI agent with a scoped toolset, run a
   named routine (a sequence of steps), or run a specific command.
4. Require human approval for chosen actions, and see every event, run,
   approval and result in an audit history.
5. Drive all of this through a server (daemon), a CLI (including a dry-run
   "would this rule fire on this event?"), and an installable web portal.
6. Install it on NixOS from a flake (package + NixOS module). Other targets
   come later.
7. **Find it easier to manage and develop than Windmill.** We learn from
   Windmill's code but deliberately stay smaller. Observable as:
   - Operating it needs one binary, one config file and one state file, with
     no database server, broker or worker fleet to run.
   - A new contributor goes from clone to running tests with
     `nix develop` and a single test command, with no second language
     toolchain.
   - Adding a new source or action type is one documented, self-contained
     change.
   - Every behaviour can be tried locally from the CLI (dry-run a rule against
     a saved event) without starting the portal.

## Affected users and systems

- The owner, as operator, and agents running on the owner's hosts.
- The owner's MCP servers (for example the aifactory/pfactory/tfactory
  servers, Backstage) as event sources and agent tools.
- REST APIs and GitHub/CI webhooks as event sources.
- The host the daemon is deployed to. No host is chosen yet, and none
  is changed without approval.
- New repo `olafkfreund/MCP-AgentGateway` (private).

## Constraints

Must:

- Treat all upstream data as untrusted. Data from sources must never become a
  shell string, and agents run with an explicit tool allowlist, turn, time and
  spend limits, and approval on by default.
- Privileged work happens only through operator-allowlisted mechanisms, never
  by giving the gateway itself root.
- Keep secrets out of the config file and the audit log: reference them as
  `env:` or `file:`, and redact before persisting.
- Prevent loops: an agent's output cannot re-trigger agents without explicit
  opt-in, and depth is capped.
- Work with servers on either MCP revision (2025-11-25 and 2026-07-28), with
  polling as the guaranteed baseline.
- Configuration is declarative and reviewable in git.
- Small footprint: runs on a single host with no external broker or database
  server.
- Not tied to one agent vendor: the agent runner is replaceable.
- Any code reused from Windmill keeps its copyright notice and licence, and
  the project licence is compatible with it. No Windmill enterprise or
  licence-gated code is ever copied.

Must not:

- Become another general MCP proxy. Proxying is out of scope unless the spec
  shows it is needed for scoping agent tools.
- Call arbitrary upstream MCP tools as "reads".
- Change production hosts without asking.

## Open questions

1. **Project licence.** Owner direction (2026-10-06): review and learn from
   Windmill's code, do not copy AGPL code, and keep our solution simpler.
   So the licence is free to choose. Proposed: **Apache 2.0**, which also
   matches the Apache parts of Windmill we may reuse (OpenFlow, clients).
   Confirm?
2. **Language/stack.** Proposed: **Go**, chosen for "easy for all to develop"
   rather than Rust (Windmill's stack). Go means the official MCP go-sdk, one
   static binary, SQLite, expr-lang rules and an htmx portal with no JS build.
   Rust's slower builds and the extra Svelte toolchain work against the
   simplicity goal. Confirm Go?
3. **Agent runner default.** `claude -p` (Claude Code headless) as the
   default runner, with others pluggable as commands. Is that acceptable?
4. **Portal scope for v1.** Read-only history plus approve/deny and rule
   enable/disable, with config still edited in git? Or should the portal also
   edit rules?
5. **Portal auth.** Bearer token in v1, OIDC through a reverse proxy later. Is
   that enough?
6. **Deployment host** for the first real run (p510 or another host).
7. **Name.** `agentgw` as the binary/CLI name?

## Resolutions (approved 2026-10-06)

Approved as drafted, with the proposed answers:

1. Licence: Apache 2.0. Learn from Windmill's code, do not copy its AGPL code.
2. Language: Go.
3. Agent runner: `claude -p` by default, with other runners pluggable as commands.
4. Portal v1: history, approve/deny and rule enable/disable. Config stays in git.
5. Portal auth: bearer token in v1, OIDC through a reverse proxy later.
6. Deployment host: decided at deployment time, not needed for spec or plan.
7. Name: `agentgw`.
