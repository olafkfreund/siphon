# Concepts

Siphon watches things, and does something when a condition holds. This page
names every moving part once. The how-tos use these words.

## The task: source → rule → action

```
 source ──event──▶ rule ──fires──▶ action
 (where)          (when)          (what)
```

What you think of as one **task** ("when a PR is opened, have an agent
review it") is these three linked parts. The portal's **Rules** page shows
each rule as that flow, and `siphon new task` creates all three at once.

### Sources: where events come from

| Type | Events arrive… | Typical use |
|---|---|---|
| `webhook` | when something POSTs to `/hook/<source>`, signed | GitHub, GitLab, EventBridge, any service that calls you |
| `http` | each `poll:` interval, from a JSON URL | metrics, status pages, REST APIs |
| `mcp` | each `poll:` interval, from an MCP resource or one read-only tool | CI, task trackers, anything with an MCP server |

An `mcp` source **without** `read` is never polled. It only gives agents
tools (for example GitHub's MCP server). An event is JSON. Rules see it as
`event`, and webhook headers as `headers`.

### Rules: when to act

A rule has:
- a `source`;
- a `when:` expression, for example `event.used_pct > 90`;
- how it fires:
  - **`on: each`** fires once per distinct `id:` (a delivery or item id).
    Use it for things that happen.
  - **`on: edge`** fires when `when` turns from false to true, and again
    every `repeat:` while it stays true. Use it for states such as "the
    disk is full".

`for_each:` splits one event into items (for example every failed task in a
list), and the rule then sees each as `item`. `cooldown:` stops a rule
firing again too soon. It's required when the action is an agent.

### Actions: what to do

| Action | Runs | Notes |
|---|---|---|
| `cmd: [argv…]` | a command in a sandbox | an argv list, never a shell; `{{.event.x}}` filled in per element |
| `agent: <name>` | an AI agent | approval required by default |
| `routine: <name>` | ordered steps (`cmd` and `agent` steps, retries, `if:`) | one rule, several things |
| `unit: <name>` | an allowlisted systemd unit | privileged work you approved in `siphon.yaml` |

## Agents and connections

An **agent** is a prompt plus limits:
- the tools it may use (`mcp:` sources and `allowed_tools`);
- `max_turns`, `max_budget_usd`, `timeout`;
- `approve`.

It runs on a **connection**:

| Connection | Agent `kind` | What it is |
|---|---|---|
| Subscription login | `claude`, `codex`, `agy` | your Claude, ChatGPT or Google subscription, used through its CLI |
| API key | `claude`, `codex` | a pay-as-you-go key |
| Model endpoint | `model` | Ollama or any OpenAI-compatible API, run by Siphon's own agent loop |

**Services** (GitHub, GitLab, AWS) are presets. They create the right
sources, connections and webhooks in one go, with safe defaults.

## Approvals

Agent actions wait for a human by default. They show in the portal's
**Approvals** page and in `siphon jobs`, and you decide with
`siphon approve <job>` or `siphon deny <job>`. Any rule or routine step can
ask for approval with `approve: true`.

## Safety you get by default

- **Sandbox.** Every action runs in its own systemd sandbox (NixOS module or
  microVM): no access to other runs, to Siphon's state, or to the host's
  secrets.
- **Egress.** Agents reach only the hosts they need (their model and their
  tools' servers), through Siphon's proxy.
- **Secrets** are never stored in the config. The config holds a reference
  (`env:NAME`, `file:/path`), and values given through the portal or CLI
  are stored write-only.
- **Two layers of config:**
  - **`siphon.yaml`**, the operator's file: the server settings and the
    privileged parts (stdio commands, private hosts, systemd units, AWS
    roles).
  - **Edits through the portal or CLI**: stored as revisions you can view
    and restore, and limited to what `siphon.yaml` allows.

## Three ways to manage it

| | Best for |
|---|---|
| **CLI** (`siphon …`) | adding and changing things, scripting, AI assistants |
| **Portal** (web) | seeing what's happening, verifying, approving, quick edits |
| **`siphon.yaml`** | the operator's baseline, kept in git |

All three change the same items, and every change made through the portal
or CLI shows in `siphon history` and on the portal's **History** page.
