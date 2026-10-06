---
status: approved
issue: 1
intent: intent/2026-10-06-1-agent-gateway.md
---

# Spec: agentgw, a self-hosted MCP/API agent gateway

Evidence: `research/2026-10-06-windmill-study.md` (Windmill code study, ideas
only, no AGPL/EE code), the landscape and architecture research, and the
Codex review summarised in the intent.

## Design

### Shape

One Go binary `agentgw`, one config file `agentgw.yaml` (the only source of
truth, kept in git), one SQLite file (runtime state only). Licence Apache 2.0.

```
                     ┌──────────────────────── agentgw serve ────────────────────────┐
 MCP server ◄─ poll ─┤ sources ──► rules ──► jobs (SQLite) ──► workers ──► actions   │
 REST API   ◄─ poll ─┤  mcp        expr       queued/running    pool N      cmd      │
 webhook  ──► POST  ─┤  http       edge/each  approval?         timeout     unit     │
                     │  webhook    cooldown                                 agent    │
                     │                                                      routine  │
                     │  :8080/ portal (htmx)   /api (JSON, used by CLI)   /hook/{src} │
                     └────────────────────────────────────────────────────────────────┘
```

The same binary is the CLI: `agentgw serve | validate | rules test | run-once |
jobs ls | approve | deny | rules enable/disable | schema`.

### Config (`agentgw.yaml`)

```yaml
server: { listen: ":8080", db: /var/lib/agentgw/state.db, workers: 4, token: file:/run/credentials/agentgw.service/token }
limits: { agent_runs_per_day: 50, http_max_body: 1MiB, http_timeout: 30s }

sources:
  factory:
    type: mcp
    url: http://p510:8090/mcp              # or command: [...] for stdio
    read: { resource: "factory://tasks/running" }   # or { tool: list_tasks, args: {...} } — operator-declared read-only
    poll: 1m
    auth: { bearer: env:FACTORY_TOKEN }     # or oauth: {...} via go-sdk
  disk:
    type: http
    url: https://metrics.local/api/disk
    poll: 5m
  github:
    type: webhook
    secret: env:GH_WEBHOOK_SECRET
    signature: github                       # preset: header, algo, encoding
    id: header.X-GitHub-Delivery

rules:
  - name: task-failed
    source: factory
    for_each: event.tasks                   # optional: split one payload into items
    id: item.id                             # identity for on: each
    when: 'item.status == "failed"'
    on: each
    cooldown: 10m
    action: { agent: triage-task }
  - name: disk-full
    source: disk
    when: 'event.used_pct > 90'
    on: edge
    repeat: 6h
    action: { unit: nix-gc.service }
  - name: pr-opened
    source: github
    when: 'headers["X-GitHub-Event"] == "pull_request" && event.action == "opened"'
    on: each
    action: { routine: review-pr }

agents:
  triage-task:
    runner: [claude, --bare, -p]            # default; any argv runner allowed
    prompt: "Task {{.item.id}} failed: {{.item.error}}. Diagnose with the factory tools and report."
    mcp: [factory]                          # sources reused as the agent's MCP servers
    allowed_tools: [mcp__factory__task_get_logs, mcp__factory__task_status]
    max_turns: 15
    max_budget_usd: 1
    timeout: 10m
    approve: true                           # default true for agents

routines:
  review-pr:
    steps:
      - { id: fetch, cmd: [gh, pr, view, "{{.event.number}}", --json, title,body] }
      - { id: review, agent: triage-task, if: 'steps.fetch.exit == 0', retry: { attempts: 2 } }
      - { id: notify, cmd: [notify-send, "PR reviewed"], continue_on_error: true }

units: [nix-gc.service]                     # allowlist for unit actions
```

A JSON Schema is generated from the Go config structs (`agentgw schema`) so
editors validate. `agentgw validate` rejects: unknown keys, agent rules without
`cooldown`, templated values in `cmd`/`unit` positions that are not whole argv
elements, units not in the allowlist, and secrets written inline instead of as
`env:` or `file:`.

### Sources

- **mcp**: official `modelcontextprotocol/go-sdk` client (stdio or Streamable
  HTTP, either protocol revision, OAuth client support). It polls exactly the
  declared `read` (a resource URI, or one operator-declared tool with fixed
  args). `subscriptions/listen` (2026-07-28) or `resources/subscribe`
  (2025-11-25) is used only as a "poll now" hint when the server supports it.
  Reconnect always does a fresh read.
- **http**: GET or POST with headers, polled.
- **webhook**: `POST /hook/<source>`. HMAC check with presets (github, generic
  sha256), constant-time compare, and a ±5 min timestamp window when the
  provider sends one. The delivery ID is stored in `seen_event` (TTL 7 days)
  to block replays. Rate limited per source.
- **One outbound HTTP client** for http and mcp sources: rejects
  private/link-local/metadata addresses after DNS unless the source sets
  `allow_private: true` (needed for LAN MCP servers), no redirects, body and
  time caps. This is Windmill's top SSRF lesson.
- A poll result becomes an **event**: `{source, received_at, headers?, event}`.
  `for_each` splits it into items. Missed polls while the gateway is down are
  not replayed; it polls once on start.

### Rules and firing semantics

- `when` is an expr-lang expression, compiled at load time, with a 100 ms eval
  timeout. Variables: `event`, `item`, `headers`, `source`.
- `on: edge`: fires on false→true per `(rule, item id or "-")`. `repeat`
  refires while still true after that interval. State lives in `rule_state`.
- `on: each`: fires once per `(rule, id)`. Seen IDs go in `seen_event`.
- `cooldown`: minimum interval between fires of a rule.
- The rule-state update and the job insert happen in **one SQLite
  transaction**.
- `agentgw rules test <rule> event.json` runs the same code path with
  `dry_run`: it prints whether the rule fires and the rendered argv/prompt.

### Jobs (queue)

- One `jobs` table: `id, rule, action_json, state(queued|pending_approval|running|done|failed|cancelled), attempt, run_after, started_at, finished_at, exit_code, output (truncated, masked), parent_id, depth`.
- Single process, so a worker claims a job with one `UPDATE … WHERE id=(SELECT … LIMIT 1) RETURNING`.
  SQLite serialises writers, so `SKIP LOCKED` is unnecessary.
- `// ponytail: no heartbeats.` One process means any `running` row at startup
  belongs to a dead run. Requeue it with `attempt+1`, or mark it failed after
  3 attempts. Add heartbeats only if multi-process ever happens.
- Retry: `attempts`, exponential with jitter (base 10 s, factor 2). Default 0
  for actions, opt-in per routine step.
- Timeout per job. Cancel sends SIGTERM, then SIGKILL after 10 s (systemd-run
  handles this with `TimeoutStopSec`).
- Global worker pool, size `server.workers`. Daily agent cap enforced at
  insert time; when it is hit, an audit row is written and the job is not
  queued.
- Retention: jobs and events older than 30 days are deleted daily.

### Actions

- **cmd**: argv array, Go `text/template` per element. A rendered value may
  not start with `-` unless the literal in config does. Run as
  `systemd-run --wait --pipe --collect -p DynamicUser=yes -p ProtectSystem=strict -p ProtectHome=yes -p PrivateTmp=yes -p NoNewPrivileges=yes -p IPAddressDeny=169.254.0.0/16 -p RuntimeMaxSec=<timeout> -- <argv>`.
  Secrets are passed via `LoadCredential`, never on argv.
- **unit**: `systemctl start --wait <unit>` for units in the `units:`
  allowlist. Privileged work lives in reviewed Nix-defined units, and the
  gateway never runs as root for it. A polkit rule in the NixOS module allows
  only those units.
- **agent**: renders the prompt and writes a temp MCP config that contains
  only the referenced sources. Runs
  `claude --bare -p <prompt> --strict-mcp-config --mcp-config <tmp> --tools "" --allowedTools <list> --permission-mode dontAsk --max-turns N --max-budget-usd X --output-format json`
  inside the same systemd-run sandbox. Output is parsed as JSON and masked.
  The runner argv is configurable. The flag set is the contract for the
  default `claude` runner.
- **routine**: ordered steps, each a `cmd`, `unit` or `agent`, with `if`
  (expr over `event`/`steps`), `retry`, `timeout`, `continue_on_error`, and
  `approve`. Each step's result is stored in the job's output. There are no
  loops or parallel branches. The names come from OpenFlow (Apache 2.0)
  without adopting its schema.
- **Loop guard**: an agent's JSON result is emitted as an event on the
  built-in source `agent-result` with `depth+1`. Only rules with
  `allow_agent_events: true` see it. Depth is capped at 2.

### Approvals

- `approve: true` puts the job in `pending_approval` and creates an
  `approvals` row with a random 32-byte token (only its hash stored) and an
  expiry (default 24 h; on expiry the job fails).
- Approve or deny through the portal, `agentgw approve|deny <id>`, or the
  one-shot link `/a/<id>/<token>`. The decision is final and written to audit.

### Audit, secrets, observability

- `audit` table: append-only rows for config load, fire, approval, job start
  and end, and cap hits.
- Secrets are resolved from `env:` or `file:` at load. Known secret values
  are replaced with `***` in any stored output or log.
- Logs go to stdout as structured JSON (`log/slog`), so journald picks them
  up. `/healthz` is the health endpoint. Metrics are left out until asked
  for.

### Portal (v1)

Go `html/template` + htmx, embedded with `embed.FS`; no JS build. Pages:
sources (last poll, error), rules (enable/disable, last fire), jobs (list,
detail, output), approvals (approve/deny), audit. Auth: bearer token from the
config (login form sets an HttpOnly cookie), with CSRF tokens on POSTs. OIDC
comes later through oauth2-proxy. Rule enable/disable is stored as a runtime
override in SQLite and shown as "overridden" so the config stays the source
of truth.

### Code layout (flat, one package per concern)

```
cmd/agentgw/main.go     CLI (stdlib flag + subcommands)
internal/config         YAML load, validate, schema, secret refs
internal/source         mcp.go http.go webhook.go netguard.go
internal/rule           expr eval, edge/each/cooldown
internal/store          SQLite (modernc.org/sqlite), migrations as embedded .sql, numbered
internal/job            queue, workers, retry, approvals
internal/action         cmd.go unit.go agent.go routine.go sandbox.go
internal/web            portal + JSON API + hook handler
nix/module.nix          NixOS module
flake.nix               package (buildGoModule), devShell, module, checks
```

Adding a source or action type means one file in `source/` or `action/` plus
one case in the config switch. There is no plugin registry.

### Nix

- `packages.default`: `buildGoModule`, CGO off.
- `devShells.default`: go, gopls, sqlite, and the go-sdk example server for
  tests.
- `nixosModules.default`: `services.agentgw.{enable, settings (rendered to
  YAML), environmentFile, credentials}`, a systemd service with
  `StateDirectory` and hardening, running as a static `agentgw` user (not
  `DynamicUser`, because polkit needs a stable user). A polkit rule lets
  that user start transient units and the allowlisted units only. The
  transient units themselves use `DynamicUser`.
- `checks`: `go test`, plus a NixOS VM test that starts the service, POSTs a
  signed webhook, and asserts a job finished.

### Delivery phases (from the approved research plan)

- **0**: config, mcp source (resource poll), rule edge/each, cmd action,
  SQLite state, CLI `validate`, `rules test`, `run-once`. No daemon.
- **1**: `serve` (scheduler, queue, workers), http source, agent action,
  approvals via CLI, loop guard, caps.
- **2**: webhook source, listen hints, portal and API, auth.
- **3**: routines, retries, unit action and polkit, NixOS module and VM test,
  JSON Schema.

## Alternatives rejected

- **Adopt or extend Windmill**: owner decision. It also needs Postgres and
  several services, has 75 crates and about 1.1M lines, and gates concurrency
  limits behind EE.
- **Copy Windmill AGPL code**: would force AGPL. Ideas are reused, code is
  not.
- **Rust (Windmill's stack)**: slower builds and a second toolchain for a
  Svelte portal. Go is easier for everyone to develop.
- **Postgres or a broker queue**: unnecessary for one host. SQLite single-writer
  makes claiming trivial.
- **Payload-hash dedup**: wrong event identity (Codex). Replaced by edge/each.
- **A custom LLM loop**: `claude -p` already provides the loop, allowlist,
  turns and budget, and the runner stays swappable.
- **Full OpenFlow schema**: loops, parallel branches and JS transforms are
  more than routines need.
- **Bidirectional git sync (Windmill CLI)**: the file is the source of truth
  and the DB holds runtime state only.
- **gojq**: unbounded recursion. `for_each` plus expr covers the shapes
  needed. Revisit if a real payload needs it.
- **nsjail or bwrap**: systemd-run is already on NixOS and gives cgroups,
  timeouts and DynamicUser. A non-systemd fallback is deferred.
- **Becoming an MCP proxy**: out of scope (intent). Agents connect to
  upstreams directly through the generated MCP config.

## Risks

| Risk | Mitigation |
|---|---|
| Prompt injection through upstream data into an agent | No built-in tools, exact MCP tool allowlist, approval by default, budget/turn/time caps, sandbox |
| Command or option injection via templates | argv-only, per-element templating, leading `-` rejected, `validate` at load |
| Cost runaway from a flapping source | `cooldown` mandatory on agent rules, daily cap, `edge` semantics |
| MCP spec churn and uneven server support | go-sdk handles both revisions; polling is the baseline and listen is only a hint |
| `allow_private` reopens SSRF for LAN MCP servers | Opt-in per source, logged at load |
| systemd-run from a DynamicUser service needs privileges | The service runs as a static `agentgw` user with a polkit rule scoped to transient units and allowlisted units. Verified in the VM test |
| Duplicate side effects after a crash mid-action | At-least-once is documented. Requeue is capped at 3, and the job shows `attempt` so operators see repeats |
| SQLite contention | WAL mode, one writer, short transactions. Single host is the design limit |

Host impact: none until a deployment host is chosen. Development and testing
use a NixOS VM.

## Verification

- `go test ./...`:
  - edge/each/cooldown/repeat tables and argv templating, including rejection
    of a leading `-`
  - webhook HMAC good/bad/replayed
  - SSRF guard blocking 169.254.169.254 and 10.0.0.0/8 unless `allow_private`
  - the startup requeue of `running` jobs
  - secret masking
- Integration test against the go-sdk example server: a changing resource
  makes `run-once` fire edge exactly once, not again while true, and again
  after true→false→true.
- `agentgw rules test` gives the same results as the live path. Both share
  one function.
- Agent action with a stub runner (a script that echoes its argv) asserts the
  exact flag contract. A real `claude` run is a manual check.
- `nix flake check` runs the unit tests plus the NixOS VM test: service
  starts, signed webhook triggers a cmd job, job is `done`, audit rows exist,
  unsigned webhook gives 401.
- DX check: in a fresh clone, `nix develop -c go test ./...` passes with no
  other setup.
