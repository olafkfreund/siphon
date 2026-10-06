---
source: windmill-labs/windmill @ 18e44d3 (2026-10-06), read-only study
licence-note: ideas and citations only; no AGPL or EE code copied
---

# Windmill study → lessons for agentgw

Repo: `.../scratchpad/windmill` (paths below relative to it). Everything cited is in CE unless marked EE.

## 1. Triggers

**What Windmill does**
- **Schedules** are not a cron daemon. After each run, the worker pushes the *next* occurrence as a queued job with `scheduled_for = next` ("chained push"), `backend/windmill-queue/src/schedule.rs:120-230`. Dedupe is a `SELECT EXISTS` on `(trigger='schedule', path, scheduled_for)` (`:177-196`). Missed occurrences are **counted and alerted on, never replayed** (`late_run_streak`, `missed_occurrences`, `:209-250`). `paused_until`, `no_flow_overlap`, `dynamic_skip` (a handler script wrapped as a first step with `stop_after_if result !== true`, `:300-330`) live on the `schedule` table (`summarized_schema.txt:184`).
- **Webhooks / HTTP triggers**: `http_trigger` row with `authentication_method ∈ {none, windmill, api_key, basic_http, custom_script, signature}` (`summarized_schema.txt:7,130`). HMAC verification supports SHA1/256/512, hex/base64, provider presets (Slack, Stripe, GitHub…) with a ±300 s timestamp window (`windmill-trigger-http/src/http_trigger_auth.rs:15-27, 84-135`). No nonce store, so replay within the window is possible. No idempotency key on job submission — I found none in `windmill-api-jobs`/`windmill-trigger-http`.
- **Polling "trigger scripts"** are ordinary scripts of `kind=trigger` (`script_kind` enum) run on a schedule; state is a `resource` of type `state` at a derived path `<script_path>/<trigger-or-user>` exposed as `WM_STATE_PATH` (`windmill-common/src/variables.rs:416-437`), read/written via `get_state/set_state` in the client (`python-client/wmill/wmill/client.py:752-770`). Dedupe is entirely the script's problem.
- **MCP**: `windmill-mcp` is an MCP *server* exposing scripts/flows as tools plus a *client* used by AI-agent steps (`windmill-mcp/src/lib.rs:1-30`). There is **no MCP trigger/resource-subscription**; `trigger_kind` enum has no mcp.
- Streaming triggers (kafka/nats/mqtt/…) use a `server_id` + `last_server_ping` lease per trigger row (`windmill-trigger/src/listener.rs:243-275`) so one server owns a listener.

**Adopt**: chained "push next occurrence" dedupe keyed on `(source, scheduled_for)`; a per-source state blob (one `source_state(source_id, key, value_json)` row) rather than a generic resource system; signature auth with timestamp window; `paused_until`; no catch-up replay by default (count+alert).
**Avoid**: per-script user-managed state and dedupe. agentgw owns dedupe centrally: `on: each` keys on upstream event id in a `seen_event(source, event_id)` table; `on: edge` compares last condition value. Add a tiny `webhook_seen(nonce/delivery-id, ts)` table — Windmill's lack of one is in its own threat model (T12).

## 2. Job queue

**What Windmill does**
- Split tables: `v2_job` (immutable definition), `v2_job_queue` (`running`, `scheduled_for`, `suspend`, `suspend_until`, `tag`, `priority`, `worker`), `v2_job_runtime` (`ping`, `memory_peak`), `v2_job_status` (flow status JSON), `v2_job_completed` (`summarized_schema.txt:208-215`).
- Claim: `SELECT id … WHERE running=false AND tag IN (…) AND scheduled_for <= now() ORDER BY priority DESC, scheduled_for FOR UPDATE SKIP LOCKED LIMIT 1` (`windmill-common/src/worker.rs:857-868`). Suspended jobs are pulled by a second query (`:832-845`).
- Heartbeat: worker updates `v2_job_runtime.ping`; a monitor requeues jobs with `ping < now()-ZOMBIE_JOB_TIMEOUT` (default 60 s), up to `RESTART_LIMIT` times via `zombie_job_counter`, else fails them (`src/monitor.rs:5919-6010`). Flow-parent jobs and `same_worker` jobs are excluded.
- **Concurrency limits are EE-only** (`windmill-queue/src/jobs.rs` pull loop: "Concurrent limits are an EE feature only" ~line 4755; `jobs_ee::apply_concurrency_limit`). Skipped.
- Retry: `constant{attempts,seconds}` / `exponential{attempts,multiplier,seconds,random_factor}` + `retry_if` expr (`openflow.openapi.yaml:132-167`). Timeout: per-job `timeout` capped by env `TIMEOUT` (`windmill-common/src/worker.rs:455`), enforced with `tokio::time::timeout` (`windmill-worker/src/handle_child.rs:827`). Cancel: `canceled_by/canceled_reason` set on queue row, worker polls and sends SIGINT→SIGTERM→kill (`handle_child.rs:274-320`).

**Adopt (SQLite)**: one `job` table with `state, run_after, locked_by, lock_expires_at, attempt, last_heartbeat`. Single process ⇒ claim is `UPDATE job SET state='running', locked_by=? WHERE id = (SELECT id … LIMIT 1)` under SQLite's writer lock; no SKIP LOCKED needed. Keep: heartbeat + zombie requeue with restart cap, priority/run_after ordering, exponential retry with jitter, per-step timeout, cancel via SIGTERM then SIGKILL (systemd-run gives this for free with `--property=TimeoutStopSec`).
**Avoid**: tags/worker groups, suspended-job second queue, four-table split, jsonb flow status. One table, one goroutine pool, cooldown handled at rule level.

## 3. Approvals

**What Windmill does**: `suspend` on a flow module: `required_events, timeout, resume_form, user_auth_required, user_groups_required, self_approval_disabled, continue_on_disapprove_timeout` (`openflow.openapi.yaml`, FlowModule.suspend). Resume/cancel URLs carry `resume_id` + HMAC-SHA256(workspace_key, job_id ‖ resume_id ‖ approver) (`windmill-api/src/jobs.rs:5700-5716`); possession of the URL *is* the authorization—identity rules are enforced only on the authenticated endpoint (`:5590-5597` comment). Replay blocked by `resume_job` row existence check (`:5598-5606`). Queue row is locked in the resume transaction to avoid a race with the worker entering suspend.

**Adopt**: `approval(id, job_id, step, expires_at, decided_by, decision, token_hash)`; one random 32-byte token per approval, store hash, URL `/a/<id>/<token>`; one-shot; timeout → fail by default, `continue_on_timeout` flag; `self_approval_disabled` by requiring approver ≠ requester when authenticated.
**Avoid**: HMAC-derived URLs with approver in the MAC (needs a workspace key; random token is simpler and revocable), `required_events>1`, resume forms, groups-expression.

## 4. OpenFlow (Apache-2.0)

`openflow.openapi.yaml` (1488 lines) defines `FlowModule{id, value, stop_after_if, skip_if, sleep, timeout, retry, continue_on_error, suspend, …}` and `FlowModuleValue ∈ {rawscript, script, flow, forloopflow, whileloopflow, branchone, branchall, aiagent, identity}`; inputs are `InputTransform` = `static` or `javascript` expr. Status is a parallel `FlowStatus` JSON per module.

**Adopt a subset only**: `steps: [{id, action, args (static | expr), if, retry, timeout, continue_on_error, approval}]` plus `on_error: stop|continue`. Use expr-lang for both `if` and arg transforms, same engine as rules.
**Avoid**: loops, branchall/parallel, raw scripts, nested flows, the JS transform language, the giant status JSON. Not worth adopting the schema; worth copying three field names (`stop_after_if`→`if`, `continue_on_error`, `retry`).

## 5. AI agent + MCP tools

**What Windmill does**: `AiAgent` module with `tools[]` where each tool is a flow module, websearch, or `mcp{resource_path, include_tools, exclude_tools}` (`openflow.openapi.yaml:999-1085`). MCP resource = `{name, url, token?, headers?}` (`windmill-mcp/src/client/types.rs:10-21`); URL is SSRF-validated and redirects disabled before the bearer token is sent (`client/mod.rs:37-85`), private URLs behind `ALLOW_PRIVATE_MCP_SERVER_URLS`. Loop: `max_iterations` default 10, hard cap 1000; hitting the cap is an error unless `continue_on_error` (`windmill-worker/src/ai_executor.rs:104-105, 1584-1587, 2101-2115`). Per-tool `job_token_scopes` can only narrow the step's token. Tool names must match `^[a-zA-Z0-9_]+$`.

**Adopt**: explicit allowlist (`include_tools`) per agent action, iteration cap with hard ceiling, tool-name regex, narrow-only scopes, SSRF-check MCP URLs at config load.
**Avoid**: a bespoke LLM loop; `claude -p --allowedTools … --max-turns … --max-budget-usd …` subprocess provides the loop, allowlist, and budget. Our agent gets secrets only via env of the sandboxed subprocess, never in logs (mask by substring, as Windmill does with Aho-Corasick, THREAT_MODEL T15).

## 6. Config-as-code

CLI syncs a workspace to a folder tree (`f/<folder>/<name>.ts` + `<name>.script.yaml`, `flow.yaml` dirs, `*.schedule.yaml`, `*.http_trigger.yaml`), controlled by `wmill.yaml` with ~35 keys (`cli/wmill.schema.json`: `includes, excludes, skip*, include*, gitBranches, promotion, codebases…`). `windmill-yaml-validator` validates those YAMLs against JSON Schema derived from OpenFlow + OpenAPI (`windmill-yaml-validator/README.md`). Sync is bidirectional with diff/pull/push (`cli/src/commands/sync`).

**Adopt**: publish one JSON Schema for `agentgw.yaml` (generated from Go structs) so editors validate; `agentgw validate` and `agentgw diff` commands.
**Avoid**: bidirectional sync—the file is the source of truth, the DB holds only runtime state (jobs, approvals, audit, source state). No `includes/excludes` zoo.

## 7. Security (THREAT_MODEL.md)

Key findings: nsjail **off by default**, only PID-ns unshare; SSRF is the #2 threat with piecemeal fixes (`T2`); `global_settings` plaintext at rest (`T6`); webhook HMAC without anti-replay (`T12`); secrets leak via `/proc` env and logs (`T15`); no rate limiting (`T18`); generated wrapper code injection (`T4`). Recommended mitigations: one SSRF-guarded HTTP client, constant-time HMAC + timestamp/nonce, pass identifiers as argv not spliced into code, secrets via files/pipes.

**Adopt**: systemd-run with `DynamicUser`, `PrivateNetwork` optional, `ProtectSystem=strict`, `IPAddressDeny` for metadata ranges; one `http.Client` with a dialer that rejects private/link-local IPs post-DNS and disables redirects; argv arrays only (no shell strings); secrets from systemd `LoadCredential`, scrubbed from audit; webhook nonce table; rate limit webhook endpoints.

## 8. Nix

`flake.nix` (574 lines): devShells `default/full/wasm/cli`, pinned clang 18 + mold, prebuilt V8 archive, bindgen clang args, dozens of C deps, language runtimes as env vars (`DENO_PATH` …), helper scripts (`wm-migrate`, `wm-reset`). No `nixosModules`, no package build of the binary itself.

**Adopt**: helper scripts via `writeShellScriptBin` in the devShell; env-var paths for external binaries (`CLAUDE_PATH`, `SYSTEMD_RUN_PATH`). **Avoid**: everything else—`buildGoModule` + `nixosModules.default` with a `systemd.services.agentgw` and `StateDirectory` should fit in <150 lines.

## 9. DX pain points (evidence)

- 75 workspace crates (`backend/Cargo.toml`), 78 cargo features; dev docs warn routes 404 when a feature isn't compiled in (`backend/AGENTS.md:23-31,111-140`).
- 1344 migration files, 3057 sqlx offline query files (`backend/migrations`, `backend/.sqlx`).
- ~495k lines Rust, ~620k lines Svelte/TS; 529 package.json lines.
- Required services: Postgres, server, worker, native worker, indexer, Caddy (`docker-compose.yml`); dev needs Docker Postgres (`start-dev-db.sh`), cargo-watch, workmux/tmux, LLM CLI for branch names (`README_WORKMUX_DEV.md`).
- Concurrency limits, autoscaling, git-sync are EE-gated; CE builds carry stubs.

## Top-10 design decisions for agentgw

1. One SQLite DB, one `job` table (`state, run_after, priority, attempt, locked_at, heartbeat_at`), claim via single UPDATE; zombie requeue after N missed heartbeats, cap restarts at 3.
2. Dedupe owned by the core: `seen_event(source_id, event_id)` for `on: each`; `(source_id, scheduled_for)` uniqueness for schedules; webhook `delivery_id` nonce table with TTL.
3. Schedules as chained "push next run" rows; missed runs counted + surfaced, not replayed.
4. Source state is a single JSON row per source, managed by agentgw, not by user code.
5. Approvals: random one-shot token (hash stored), expiry, `continue_on_timeout`, requester≠approver.
6. Routine = flat ordered steps with `if`, `args` exprs, `retry{attempts, base, factor, jitter}`, `timeout`, `continue_on_error`; no loops/branch-all.
7. All execution through `systemd-run` with argv arrays, `DynamicUser`, `LoadCredential`, optional `PrivateNetwork`; never shell strings.
8. One outbound `http.Client`: SSRF-safe dialer, no redirects, size/time caps—shared by REST sources, webhooks, MCP.
9. Agent action = `claude -p` with `--allowedTools` allowlist, `--max-turns`, budget; output masked for known secret values before audit/log.
10. `agentgw.yaml` is the only config; ship a generated JSON Schema; DB holds runtime state only; NixOS module exposes `settings` as that YAML.

## Could not find / verify

- Exact exponential-retry delay formula (settings struct found in `windmill-common/src/runnable_settings/settings.rs:178-188`; computation site not located).
- Webhook idempotency key on job submission—appears absent.
- Concurrency-limit algorithm (EE, `jobs_ee`, not in repo).
- Whether HTTP-trigger HMAC compares in constant time (uses `hmac::Mac::verify_slice`, which is constant time, but I did not audit all presets).
- Any MCP-based trigger/subscription—none exists.