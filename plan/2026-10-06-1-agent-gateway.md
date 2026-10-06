---
status: approved
issue: 1
spec: spec/2026-10-06-1-agent-gateway.md
---

# Plan: agentgw, a self-hosted MCP/API agent gateway

## Approved decisions (self-contained; do not need the intent or spec to implement)

- **Product:** `agentgw`, a single Go binary that is daemon, CLI and portal. Licence Apache 2.0.
  - Config is one YAML file, `agentgw.yaml`, and it is the only source of truth.
  - Runtime state lives in one SQLite file: jobs, rule state, seen events, approvals, audit and rule overrides. No other DB, broker or service.
- **Stack:**
  - Go 1.26 (nixpkgs `go` 1.26.8).
  - `github.com/modelcontextprotocol/go-sdk` v1.8.0 (`mcp` package).
  - `github.com/expr-lang/expr` v1.17.x.
  - `modernc.org/sqlite` (CGO off, driver name `sqlite`).
  - `gopkg.in/yaml.v3`.
  - stdlib `flag`, `log/slog`, `net/http`, `html/template`, `embed`.
  - htmx vendored as one static file.
  - Nothing else unless a step says so.
- **Sources:**
  - `mcp` (stdio `command:` or HTTP `url:`) polls exactly one declared `read`: a resource URI, or a tool plus fixed args that the operator declared read-only. `subscriptions/listen` or `resources/subscribe` is only a "poll now" hint, and a reconnect always does a fresh read.
  - `http` is a polled GET or POST.
  - `webhook` is `POST /hook/<source>` with an HMAC preset (`github`, `sha256`), constant-time compare, a ±5 min timestamp window when present, and the delivery ID stored in `seen_event` (TTL 7 days) to block replay. Rate limited per source.
  - All outbound HTTP goes through one guarded client: private, link-local and metadata IPs are rejected after DNS unless the source sets `allow_private: true` (logged at load). No redirects. Body cap (`limits.http_max_body`, default 1 MiB) and timeout (`limits.http_timeout`, default 30 s).
  - An event is `{source, received_at, headers, event}`. Optional `for_each: <expr>` splits it into `item`s. Missed polls are not replayed; each source polls once on start.
- **Rules:**
  - `when` is an expr-lang expression compiled at load, with a 100 ms eval timeout. Variables: `event`, `item`, `headers`, `source`.
  - `on: edge` fires on false→true per `(rule, key)`, where the key is `id` (an expr) if set, else `"-"`. `repeat` refires while the condition stays true.
  - `on: each` fires once per `(rule, id)`.
  - `cooldown` is the minimum interval between fires. It is **mandatory** on rules whose action is an agent, or a routine that contains one.
  - The state update and job insert happen in one transaction.
  - `agentgw rules test` uses the identical function with `dryRun=true`.
- **Jobs:**
  - One `jobs` table. Claim with a single `UPDATE … RETURNING`.
  - No heartbeats: at startup, every `running` job is requeued with `attempt+1`, or failed once `attempt` reaches 3.
  - Retry is opt-in: exponential with jitter, base 10 s, factor 2.
  - Per-job timeout. Cancel sends SIGTERM, then SIGKILL after 10 s.
  - Global worker pool of `server.workers` (default 4).
  - Daily agent cap (`limits.agent_runs_per_day`, default 50) is checked at insert; when hit, write an audit row and skip the job.
  - Rows older than 30 days are deleted daily.
- **Actions:**
  - `cmd`: an argv array rendered per element with `text/template`. A rendered element must not start with `-` unless the literal config element does. Runs under `systemd-run --wait --pipe --collect --quiet` with DynamicUser, ProtectSystem=strict, ProtectHome, PrivateTmp, NoNewPrivileges, `IPAddressDeny=169.254.0.0/16` and `RuntimeMaxSec`. Secrets go via `LoadCredential`, never argv. Never `sh -c`.
  - `unit`: `systemctl start --wait <u>`, only for units in `units:`.
  - `agent`: renders the prompt and writes a temp MCP config containing only the referenced sources. Default runner: `claude --bare -p <prompt> --strict-mcp-config --mcp-config <tmp> --tools "" --allowedTools <csv> --permission-mode dontAsk --max-turns N --max-budget-usd X --output-format json`, run inside the same sandbox. The runner argv is configurable. `approve` defaults to true.
  - `routine`: ordered steps (`cmd`, `unit` or `agent`) with `if`, `retry`, `timeout`, `continue_on_error` and `approve`. Step results are available as `steps.<id>.{exit,output,json}`.
  - Loop guard: an agent's JSON result becomes an event on the built-in source `agent-result` with `depth+1`. It only matches rules with `allow_agent_events: true`. Maximum depth is 2.
- **Approvals:** `pending_approval` state plus an `approvals` row. The token is 32 random bytes; only its SHA-256 is stored. Default expiry 24 h, after which the job fails. Decide through the portal, `agentgw approve|deny <id>`, or the one-shot link `/a/<id>/<token>`. Every decision is audited.
- **Secrets:** only `env:NAME` or `file:/path` refs, resolved at load. Inline secret-looking values are rejected. Known secret values are masked as `***` in stored output and logs.
- **Portal:**
  - Pages: sources, rules (enable/disable as a runtime override shown as "overridden"), jobs, approvals and audit.
  - Auth: a bearer token from config. The login form sets an HttpOnly, SameSite=Strict cookie; POSTs carry CSRF tokens.
  - `/api/*` serves JSON with `Authorization: Bearer`. `/healthz` needs no auth.
- **Layout:**
  - `cmd/agentgw`
  - `internal/{config,source,rule,store,job,action,web}`
  - `nix/module.nix`
  - `flake.nix`
  - Adding a source or action type is one file plus one switch case. No plugin registry, and no interfaces with a single implementation.
- **Nix:**
  - `packages.default` built with `buildGoModule` (CGO off).
  - `devShells.default` with go, gopls, sqlite and the go-sdk example server.
  - `nixosModules.default` exposes `services.agentgw.{enable,settings,environmentFile,credentials}`. The service runs as a static `agentgw` user with `StateDirectory` and hardening, plus a polkit rule that allows only transient units and the allowlisted units.
  - `checks` holds the go tests and a NixOS VM test.

## Steps

Each step is a single commit. Cite "Plan step N" in the commit body. Run `nix develop -c go test ./...` before every commit after step 1.

### Phase 0: configuration, MCP polling, rules, cmd action, CLI only

1. **`flake.nix`, `go.mod`, `LICENSE`, `README.md`, `.gitignore`, `cmd/agentgw/main.go`.** Scaffold the repo.
   - `go mod init github.com/olafkfreund/MCP-AgentGateway`.
   - The flake has a devShell with `go_1_26` (or `go`), `gopls` and `sqlite`, and `packages.default = buildGoModule { CGO_ENABLED = 0; vendorHash = …; }`.
   - `main.go` dispatches subcommands with the stdlib `flag` package: `validate`, `rules`, `run-once`, `version`.
   - Verify: `nix develop -c go build ./... && nix build && ./result/bin/agentgw version`.
   - Traps: `vendorHash` starts as `lib.fakeHash`; copy the real hash from the build error. No cobra or other CLI library.
2. **`internal/config/config.go`, `internal/config/config_test.go`.** Write the config structs that mirror the spec YAML: server, limits, sources, rules, agents, routines and units.
   - Load with yaml.v3 and `KnownFields(true)`.
   - Resolve `env:` and `file:` refs and collect secret values for masking.
   - `Validate()` returns every error at once. It covers:
     - unknown source, agent or routine references
     - agent rules without `cooldown`
     - templated values in `units` entries
     - unit actions not in the `units:` allowlist
     - inline secrets, meaning any `secret`/`token`/`bearer` value not prefixed `env:` or `file:`
     - `when`/`id`/`for_each` expressions that fail to compile
   - Verify: table tests for each validation error. `agentgw validate testdata/full.yaml` exits 0, and `testdata/bad.yaml` exits 1 listing all errors.
   - Traps: compile expr here so load fails fast (`expr.Compile(src, expr.Env(map[string]any{}), expr.AllowUndefinedVariables())`). Durations use a custom `Duration` type that unmarshals from strings like "5m".
3. **`internal/store/store.go`, `internal/store/migrations/0001_init.sql`.** Write the SQLite layer.
   - Open with `modernc.org/sqlite` and pragmas WAL, `busy_timeout=5000` and `foreign_keys=on`, using `db.SetMaxOpenConns(1)` for writes.
   - Migrations are embedded `NNNN_*.sql` files applied in order, tracked in `schema_migrations`.
   - Tables: `jobs`, `rule_state(rule,key,last_value,last_fired_at)`, `seen_event(scope,id,seen_at)`, `approvals`, `audit`, `rule_override`, `source_state(source,json,last_poll_at,last_error)`.
   - Verify: a test opens a temp DB twice, so migrations are idempotent, and checks all tables exist.
   - Traps: the driver name is `"sqlite"`, not `"sqlite3"`. No ORM and no sqlc; use hand-written queries in this package only.
4. **`internal/rule/rule.go`, `internal/rule/rule_test.go`.** Write `Evaluate(ctx, tx, rule, ev, now, dryRun) ([]Fire, error)`.
   - Apply `for_each`, then for each item: `when` with a 100 ms context timeout, then edge/each/cooldown/repeat logic against `rule_state` and `seen_event`.
   - Return fires, each with a rendered key.
   - Verify: table tests for edge (false→true fires; true→true does not; refires after `repeat`; true→false→true fires), each (the same id fires once), and cooldown.
   - Traps: all state writes go through the passed `tx`. The caller inserts jobs in the same `tx`. expr needs `expr.Env` built from a `map[string]any`.
5. **`internal/source/netguard.go`, `internal/source/http.go`, `internal/source/mcp.go` and their tests.** Write the sources.
   - `netguard`: an `http.Client` with a custom `DialContext` that resolves, then rejects loopback, private, link-local, CGNAT, metadata and unspecified IPs unless `allowPrivate`. `CheckRedirect` returns `http.ErrUseLastResponse`. Wrap the body in `io.LimitReader`.
   - `http`: `Poll(ctx) (Event, error)`.
   - `mcp`: `mcp.NewClient(&mcp.Implementation{Name:"agentgw"}, nil)`. Transport is `&mcp.CommandTransport{Command: exec.Command(...)}` for `command:`, or the streamable client transport with the guarded `http.Client` for `url:`. Use `session.ReadResource` for `read.resource` and `session.CallTool` for `read.tool`. Content is parsed as JSON when its mime type or text parses, else `{"text": …}`.
   - Verify:
     - netguard tests reject `169.254.169.254` and `10.1.2.3` and allow them with `allowPrivate`.
     - The mcp test starts an in-process go-sdk server with one resource, using `mcp.NewInMemoryTransports()`, and reads it.
   - Traps: check the go-sdk v1.8.0 API names against `go doc github.com/modelcontextprotocol/go-sdk/mcp`; do not guess. Bearer auth goes in through an `http.RoundTripper` that adds the header. MCP sources must never call any tool other than the declared `read.tool`.
6. **`internal/action/sandbox.go`, `internal/action/cmd.go` and their tests.**
   - `Render(argv []string, data any) ([]string, error)` renders per element and rejects a leading `-` introduced by templating.
   - `SandboxArgv(argv, timeout, creds)` builds the systemd-run prefix from the decisions section.
   - `RunCmd(ctx, argv) (exit int, output []byte)` captures combined output truncated to 64 KiB and applies secret masking.
   - A config flag `server.sandbox: systemd|none` (default systemd) exists for tests and dev; `validate` warns when it is `none`.
   - Verify: tests for rendering (an injected `-rf` value is rejected), the exact sandbox argv, and masking. Run one real `RunCmd` with sandbox `none`.
   - Traps: never pass anything through a shell. `exec.CommandContext` plus `cmd.Cancel` sends SIGTERM, with `WaitDelay=10s`.
7. **`cmd/agentgw/main.go` (`rules test`, `run-once`).** Wire up the CLI.
   - `agentgw rules test <rule> <event.json>` loads config, evaluates against an in-memory DB with dryRun, and prints the fires and rendered argv as JSON.
   - `agentgw run-once` polls every source once against the real DB file, then evaluates rules, inserts jobs and runs `cmd` jobs inline.
   - Verify: the integration test in `internal/e2e_test.go` uses the in-memory MCP server with a mutable value and calls `runOnce` repeatedly, asserting edge semantics end to end and that a job row is `done`.
   - Traps: `run-once` and `serve` (step 8) must call the same `pipeline.Tick` function. Put it in `internal/job/pipeline.go` now.

### Phase 1: daemon, HTTP source, agent action, approvals, loop guard

8. **`internal/job/queue.go`, `internal/job/pipeline.go`, `cmd/agentgw` (`serve`).** Build the daemon loop.
   - A per-source ticker goroutine calls `Tick` at the poll interval.
   - The worker pool claims jobs with `UPDATE jobs SET state='running', started_at=? WHERE id=(SELECT id FROM jobs WHERE state='queued' AND run_after<=? ORDER BY id LIMIT 1) RETURNING …`.
   - Startup requeue follows the decisions section. Opt-in retry with backoff. Daily retention cleanup. `context` cancellation on SIGINT/SIGTERM waits for workers.
   - Verify: tests for claim under 8 concurrent workers (every job runs exactly once), startup requeue (`attempt` increments, and the job fails after 3), and retry backoff timings with an injected clock.
   - Traps: inject `now func() time.Time` everywhere; never call `time.Now()` directly in the logic.
9. **`internal/action/agent.go` and its test.** Build the agent action.
   - Write the MCP config JSON (only the referenced sources: their `url`/`command` and auth headers via env) to a 0600 temp file in the job dir.
   - Build the runner argv from the decisions section, run it via the sandbox, and parse the JSON output.
   - Emit an `agent-result` event with `depth+1`.
   - Verify: a test with a stub runner script (`testdata/stub-runner.sh` echoes its argv as JSON) asserts the exact flag contract and that the temp config holds only the allowed sources.
   - Traps: do not put secrets in argv. Pass them to the sandbox via `LoadCredential` or env from the credentials dir. `--tools ""` must be a separate empty argv element.
10. **`internal/job/approval.go` and `cmd/agentgw` (`jobs ls`, `approve`, `deny`).** Build approvals.
    - Jobs with `approve` go to `pending_approval` with an approvals row (token hash and expiry).
    - Expiry is swept every minute.
    - The CLI talks to the local DB when run on the same host, and later (step 13) to `/api` when `--server` is given.
    - Verify: tests for approve→queued, deny→cancelled, expiry→failed, a second decision rejected, and audit rows written.
    - Traps: generate tokens with `crypto/rand`. Compare token hashes with `subtle.ConstantTimeCompare`.
11. **`internal/rule` and `internal/job` (loop guard and caps).** Add `allow_agent_events` filtering, enforce the depth cap of 2 at insert, and enforce the daily agent cap.
    - Verify: tests showing an agent-result event ignored by default and accepted with opt-in, depth 3 rejected and audited, and the 51st agent job of the day skipped and audited.
    - Traps: none.

### Phase 2: webhooks, listen hints, portal and API

12. **`internal/source/webhook.go` and its test.** Build the webhook handler.
    - `POST /hook/{source}` verifies the HMAC: the `github` preset uses header `X-Hub-Signature-256`, `sha256=<hex>`, over the raw body; `sha256` is a generic preset with a configurable header.
    - Apply the timestamp window when configured.
    - Replay check via `seen_event(scope='hook:'+source, delivery id)`.
    - Per-source token-bucket rate limit (stdlib only, 10/s burst 20).
    - Returns 202 and feeds `pipeline.HandleEvent`.
    - Verify: tests for a good signature (202), a bad one (401), a replay (409) and the rate limit (429).
    - Traps: read the body once with a size cap before verifying. Use `hmac.Equal`.
13. **`internal/web/` (`server.go`, `api.go`, `portal.go`, `templates/*.html`, `static/htmx.min.js`).** Build the HTTP server, API and portal.
    - One `http.ServeMux` with Go 1.22+ patterns.
    - `/api/{sources,rules,jobs,approvals,audit}` as JSON, plus POST for approve, deny and rule enable/disable.
    - Portal pages for the same views using htmx partials.
    - Bearer auth for `/api`, a cookie login for the portal, CSRF tokens on POST, `/healthz`.
    - Verify: `httptest` tests for 401 without a token, approve via API changing the job state, a POST without CSRF giving 403, and the rule override shown as "overridden".
    - Traps: vendor htmx into `static/` and keep the version in a comment. No CDN, no JS build. Templates go through `embed.FS`.
14. **`internal/source/mcp.go` (listen hints).** When the session supports it, open `subscriptions/listen` (2026-07-28) or `resources/subscribe` (2025-11-25) for the declared resource. An update triggers an immediate `Tick` for that source, debounced to 5 s. On error, fall back silently to polling and log once.
    - Verify: the in-memory server test changes the resource, notifies, and asserts a tick happens before the poll interval.
    - Traps: check the go-sdk v1.8.0 API for subscriptions with `go doc`. If the API differs, keep polling only and record the deviation in this plan.

### Phase 3: routines, unit action, packaging

15. **`internal/action/routine.go`, `internal/action/unit.go` and their tests.**
    - Routine: steps run in order; `if` is evaluated with expr over `{event, item, steps}`; plus per-step retry, timeout, `continue_on_error`, and `approve` (which pauses the routine job and resumes at that step after approval). Stored output is `{steps: {id: {exit, output, json}}}`.
    - Unit: `systemctl start --wait <unit>`, with the allowlist enforced again at runtime.
    - Verify: a routine with 3 steps where step 2 fails with `continue_on_error`, an `if` skip, and approval in the middle with resume; a unit outside the allowlist is rejected at runtime too.
    - Traps: resuming after approval stores `resume_step` in the job row. Do not re-run completed steps.
16. **`cmd/agentgw` (`schema`).** Generate JSON Schema from the config structs by reflection (stdlib `reflect`, about 100 lines). Commit `schema/agentgw.schema.json` and have a test that fails if it is stale.
    - Verify: the test passes. A deliberate struct change without regenerating fails it.
    - Traps: no schema library dependency.
17. **`nix/module.nix`, `flake.nix` (module and checks).** Build the NixOS module.
    - Options: `services.agentgw.{enable, package, settings (freeform YAML via pkgs.formats.yaml), environmentFile, credentials (attrset name→path → LoadCredential)}`.
    - Static user and group `agentgw`, `StateDirectory=agentgw`, hardening (`ProtectSystem=strict`, `ProtectHome`, `NoNewPrivileges` is **not** set because systemd-run needs polkit, `PrivateTmp`, `RestrictAddressFamilies`).
    - A polkit rule that allows `org.freedesktop.systemd1.manage-units` for user `agentgw` only for transient `run-*` units and the `units:` allowlist.
    - Verify: `nix flake check` passes.
    - Traps: `ExecStart` uses `${cfg.package}/bin/agentgw serve --config ${configFile}`. Do not put secrets in `settings`; that ends up world-readable in the store.
18. **`nix/vm-test.nix`, wired into `checks`.** Write the NixOS VM test.
    - The node enables the module with a webhook source and two rules: one with a `cmd` action (`[true]`, run sandboxed through systemd-run), and one with a `unit` action starting an allowlisted oneshot unit that touches a marker file.
    - The test POSTs a signed webhook with curl, then waits until `agentgw jobs ls` shows both jobs `done` and the marker file exists. An unsigned POST returns 401. `/healthz` returns 200.
    - Verify: `nix build .#checks.x86_64-linux.vm`.
    - Traps: the VM test runs in a sandbox with no network; use a local webhook only.
19. **`README.md`, `examples/agentgw.yaml`, `docs/adding-a-source.md`.** Write a quick start (`nix run`, `validate`, `rules test`, `serve`), a full example config, the "add a source or action type in one file" guide, and a security notes section.
    - Verify: in a fresh clone, `nix develop -c go test ./...` passes and the README commands run as written.
    - Traps: none.

## Tests

- `nix develop -c go test ./...` passes after every step. It covers config validation, rule semantics, netguard, argv rendering and injection rejection, the sandbox argv, masking, queue claim and requeue, approvals, the loop guard and caps, webhook HMAC, replay and rate limit, the API and portal auth, routines, and schema staleness.
- `internal/e2e_test.go` covers the in-memory MCP server through `run-once` to edge fires to a `done` job.
- `nix flake check` runs the package build, go tests and the NixOS VM test.
- Manual, before the PR: one real `claude` agent run against a local MCP server with an allowlist of one read-only tool. Check that the output is masked and that the `agent-result` event does not re-trigger.
  - **Done 2026-10-06 (after merge), on the owner's Claude subscription rather than an API key.** Runner `[claude, -p]` (no `--bare`, which ignores OAuth), `sandbox: none`, against a one-tool stdio MCP demo server. Results:
    - The `degraded` rule fired, and the agent made 2 turns with one `get_status` call and no permission denials, in about 8 s.
    - Its JSON result became an `agent-result` event that fired an `allow_agent_events` rule exactly once (depth 1, parent_id set), with no re-trigger.
    - Claude reported an estimated cost of $0.06; on the subscription it counts toward plan usage, not API billing.

## Handoff and review

- **Owner decision (2026-10-06): coding is split between Claude and OpenAI Codex.**
  - The Claude side is the `coder` agent (Sonnet), plus the session model (Opus) for integration steps.
  - Codex runs via `codex exec -s workspace-write`. The sandbox is never bypassed and the network stays off, so step 1 pre-fetches every Go dependency.
  - Codex works in its own git worktree, `../MCP-AgentGateway-codex`, on branch `feat/1-codex-lane`, and does not commit. Opus reviews its diff, runs the tests, and commits it with Codex attribution, then merges the lane into `feat/1-agent-gateway`.
- Split rule: Codex takes self-contained packages with a clear interface; Claude takes the core path and the integration. Lanes touch disjoint directories, so they run in parallel.

| Phase | Opus (session) | `coder` (Claude Sonnet) | Codex |
|---|---|---|---|
| 0 | 1 (scaffold, deps), 7 (integration) | 2 → 3 → 4 (`config`, `store`, `rule`) | 5 → 6 (`source`, `action/cmd+sandbox`) |
| 1 | 11 | 8, 10 | 9 (`action/agent`) |
| 2 | — | 13 (`web`) | 12 (`source/webhook`), 14 (listen hints) |
| 3 | 17, 18 (Nix module, VM test) | 15, 19 | 16 (`schema`) |

- Start one `coder` per task with this plan path and its first step. Send each later step to the same agent with `SendMessage`.
- After each phase, a fresh Opus reviewer gets only this plan path and `git diff` for that phase. This covers both Claude's and Codex's code, so each model's work is checked by another.
- One PR per phase into `main`, each linking intent, spec and plan, and stating which steps the coder did.
- Any deviation updates this file in the same commit as the code.

## Rollback

- Greenfield repo; nothing is deployed. To roll back a step, `git revert` its commit; each step is one commit.
- No host is changed by this plan. Deploying the NixOS module to a real host is a separate, approved change; it is removed by disabling `services.agentgw.enable` and rebuilding. The state is `/var/lib/agentgw` and can be deleted.

## Deviations log

- Step 2:
  - `auth.oauth` on MCP sources is deferred; v1 supports `auth.bearer` only (the spec listed OAuth via go-sdk). `KnownFields` rejects `oauth` until a later step adds it.
  - Added source fields `method`, `headers`, `body`, `signature_header` and `timestamp_header`, and `rule.approve`.
  - `Load` does not fail on an unresolved `env:`/`file:` ref; `Validate` reports it, so all errors are listed at once.
- Step 4:
  - **Cooldown defers rather than drops.** A fire suppressed by cooldown leaves rule state untouched, so an edge that is still true, or an unseen `each` id, fires once the cooldown expires. The plan left this open.
  - `dryRun` uses a SAVEPOINT rolled back inside `Evaluate`.
  - An item whose expression errors is skipped and its error returned, without blocking the other items.
  - On a `when` timeout, the expr goroutine is abandoned, bounded by expr's memory budget.
  - Added the `store.RuleLastFired` helper.
- Step 5/6 (Codex):
  - `action.RunCmd` takes `SandboxOptions` and secret values explicitly and returns an exec error.
  - The guarded client sets an overall `http.Client.Timeout` and a body cap. Step 14 needs a separate client without these for long-lived listen streams.
- Step 7:
  - The end-to-end test lives at `internal/job/e2e_test.go`, not `internal/e2e_test.go` (`internal/` has no package).
  - Added `store.ClaimJob`, `store.FinishJob` and `store.PutSourceState`, plus `server.db` default `agentgw.db`.
  - The `cmd` action timeout is fixed at 10 min (`job.cmdTimeout`) until a rule needs its own.
  - Approval-required jobs are inserted as `pending_approval`; their approvals rows and decisions arrive in step 10.
  - `unit`/`agent`/`routine` jobs fail with "not implemented yet" until their phases.
  - Running `sandbox: systemd` as a non-root user needs the polkit rule from step 17, so local `run-once` uses `sandbox: none` until then.
- Phase 0 review (fresh Opus reviewer; 0 critical, 2 high, 5 medium, 5 low). All high and medium findings fixed, plus L1, L2, L3 and L5. Changes beyond the plan:
  - JSON is decoded with `source.DecodeJSON`, keeping integers as int64 (H1).
  - Children get only PATH/HOME/LANG (M1).
  - argv[0] can never be templated (M2).
  - netguard also blocks NAT64, 6to4, IPv4-compatible addresses, 192.0.0.0/24, 198.18.0.0/15 and the Azure/Alibaba metadata IPs (M3).
  - Source `headers` are secret-capable, and inline secret-looking headers are rejected (M4).
  - `on: each` dedupes via `rule_state`, not `seen_event`, so the TTL sweep cannot replay (M5).
  - Cancelled runs leave jobs for the startup requeue (H2). **Step 8's startup requeue must run in `RunOnce` as well as `serve`.**
  - `FinishJob` only transitions `running` jobs (L5).
  - Errors in `source_state` and on stderr are masked (L1).
- Not in the original step-2 list: the `Source.ID` field (webhook delivery id). Cooldown is per rule across all keys, so N new ids with cooldown C take (N-1)·C to all fire. This is documented in `rule.go`.
- Step 8:
  - **Retry/backoff moves to step 15.** Only routine steps carry `retry`, so Phase 1 has nothing to retry.
  - The startup requeue (`store.RequeueRunning`, max 3 attempts) runs in both `Serve` and `RunOnce`.
  - A source poll failure is logged and the loop continues.
  - Retention also removes approvals of purged jobs and nulls `parent_id` on surviving children.
  - Workers are woken by a nudge channel plus a 1 s idle poll.
- Step 9/11 (agent auth): `claude --bare` authenticates only via `ANTHROPIC_API_KEY` or an `apiKeyHelper` passed with `--settings` (per `claude --help`). In the systemd sandbox, the API key file goes in via `LoadCredential`, and a generated settings JSON sets `apiKeyHelper` to read `/run/credentials/<unit>/<name>`. This is wired in step 11.
- Step 10:
  - The approval id is the job id (one approval per job): `approve|deny <job id>` and `/a/<job id>/<token>`.
  - The one-shot link (with its token) is logged at slog info after commit, so the operator can see it. The audit row stores `/a/<id>/***`.
  - Smoke-tested by hand: `run-once`, then `jobs ls`, then `approve` (a second approve is rejected), then `run-once` again, ending in a `done` job.
- Step 9 (Codex): `RunAgent` returns the result. Emitting the `agent-result` event happens in the pipeline (step 11). `SandboxOptions.Unit` was added for explicit unit names, so the MCP config reaches the DynamicUser sandbox as `LoadCredential` at `/run/credentials/<unit>/mcp.json`.
- Step 11:
  - The agent's `mcp.json`, which may hold bearer headers, is deleted after every run.
  - New agent field `api_key_file`. It is passed as credential `api-key` with `--settings {"apiKeyHelper":"cat <path>"}`, and the path is restricted to `^/[A-Za-z0-9/._-]+$` because apiKeyHelper runs through a shell.
  - The depth cap (`config.MaxDepth`=2) and the daily agent cap (a rolling 24 h window) are enforced at enqueue, audited as `skip_depth` and `skip_agent_cap`.
  - Agent-result rule errors are logged and do not fail the agent job.
  - The manual real-`claude` check is still open: it needs the owner's API key.
- Phase 1 review (fresh Opus reviewer; 0 critical, 1 high, 6 medium, 6 low). Fixed:
  - **H1:** the agent prompt goes on **stdin**, never argv. Event data rendered into the prompt could otherwise inject `claude` flags such as `--mcp-config=<inline json>`.
  - **M1:** a job that completed during cancellation is recorded, not re-run.
  - **M2:** an flock on `<db>.lock` stops `serve` and `run-once` from sharing a DB.
  - **M3:** agent timeout defaults to 10 min; negative durations are rejected.
  - **M4:** agent stdout is captured separately, and a warning is logged when it isn't JSON.
  - **M5:** approval tokens are never logged; delivering the link is decided in step 13.
  - **M6:** a relative `server.db` resolves against the config directory, and the agent work dir is removed after every run.
  - **L1:** `poll > 0`, and agent `mcp` entries must be `type: mcp`.
  - **L5:** `parent_id` is set on agent-result jobs.
  - **L6:** a test covers agent MCP scoping.
  - The sandbox sets `HOME=/tmp` so `claude` can run under DynamicUser.
- Carried forward from that review:
  - **L2:** a cap or depth skip consumes the rule's edge/each state (dropped, not deferred). This is documented here; revisit if it matters.
  - **L3:** the daily cap counts jobs, not attempts, and step 15 must count routines that contain agents.
  - **L4:** step 13 must rate-limit `/a/` bad-token attempts.
  - **Step 17:** the polkit rule must also restrict transient-unit properties (no `User=`, credential paths only under allowed dirs), not just the unit name.
  - **Step 18:** the VM test must assert that SIGTERM to agentgw stops its transient units (no orphaned agents).
- Phase 2 decisions (before the code):
  - **The one-shot `/a/<id>/<token>` link is not built.** Tokens are never logged (Phase 1 M5), so nothing could deliver one. Approvals go through the authenticated portal/API and the CLI. The store's token path stays for a future notification action. Review carry-over L4 becomes rate-limiting of login/API auth failures.
  - **Webhook replay** is checked by the integrator's `Deliver` callback: `seen_event` MarkSeen in the same tx as `HandleEvent`, so the replay record and the enqueue commit together. `internal/source` only verifies.
  - **Portal session:** the cookie holds an HMAC of the token with a per-process random key, so a restart logs users out. CSRF uses an HMAC of the session.
- Step 13:
  - A rule override only disables. Enabling deletes the override row and restores the config default, so "overridden" means "disabled at runtime".
  - htmx 2.0.4 is vendored, byte-identical to unpkg (sha256 verified), with `allowEval:false` and no indicator styles so the CSP stays `default-src 'self'`.
  - Failed login and API auth share a per-IP bucket of 5/min. It keys on RemoteAddr only, so behind a reverse proxy all clients share one bucket (`ponytail:` note).
  - Plain POSTs redirect, so the portal works without JS.
- Step 12/14 integration (Opus):
  - **The webhook replay key is the SHA-256 of the signed body, not the delivery-id header.** Delivery-id and timestamp headers aren't covered by the HMAC, so a captured request could be replayed with a fresh id. The delivery id stays in `headers` for rules and tracing. Replay record and enqueue commit in one tx (`handleEvent` with a seen scope).
  - Listen hints trigger an immediate tick; a hint within 5 s of the last tick is dropped (the next poll catches it). Listen gives up quietly on `ErrListenUnsupported` and reconnects after the poll interval on other errors.
  - `serve` runs the HTTP server (`ReadHeaderTimeout` 10 s) and shuts it down gracefully. HandleEvent skips rules disabled via the portal/API.
  - Smoke-tested live: healthz 200, API 401 without token, signed webhook 202 → job `done`, CSP header present, a second `serve` refused by the lock, SIGTERM exits 0.
- Phase 2 review (fresh Opus reviewer; 0 critical, 3 high, 8 medium, 7 low). Opus lane fixes:
  - **H1:** `serve` binds first, and a failing HTTP server cancels the workers. Workers stop first, then `srv.Shutdown` waits for in-flight handlers before the store closes.
  - **H3:** Read/Write/Idle timeouts are set on the HTTP server.
  - **M3:** per-rule errors after commit are logged and the webhook still gets 202.
  - **M5:** listen hints use a trailing-edge debounce, so a hint is deferred rather than dropped, and the clock is injected.
  - **L2:** `job.New` creates the nudge channel before HTTP starts.
- Plan items not built (L7):
  - The step-10 CLI `--server` (talking to `/api`) is dropped. The local-DB CLI plus portal/API cover it; YAGNI.
  - The plan's `web/server.go` is `web/web.go`.
  - "byte-identical" htmx means identical below the added header comment (official SRI hash verified by the reviewer).
  - Bug found by the step-14 pipeline test (review L7): `MCP.Listen` blocked in `session.Wait()` ignoring ctx, so `serve` would hang on SIGTERM with an active subscription. The session is now closed on ctx cancel. go-sdk v1.8.0 uses `subscriptions/listen` (2026-07-28) under `Subscribe`, per the stack trace.
  - `listenDebounce` is a package var so tests can shorten it.
- Step 15:
  - Routine execution lives in `internal/job/routine.go`, not `internal/action`, because it needs the pipeline's agent/cmd runners.
  - Progress `{steps, awaiting}` lives in the job `output` column, with `resume_step` as the next index and no migration. It is saved after every step, so a crash re-runs only the interrupted step.
  - `retry.attempts` is total tries, with backoff `base*factor^n` (10 s, ×2, capped at 1 h) and ±20 % jitter.
  - Each `approve` step creates a new approvals row, and `DecideApproval` uses the latest row.
  - The payload carries `agent: true` for agent actions and agent-containing routines, so the daily cap counts both (Phase 1 L3).
  - **The unit action was written by Opus, not the coder:** the coder agent's command guard blocks any command whose text contains `systemctl start`. The owner chose this.
  - The allowlist is checked three times: in config, at runtime (`action.RunUnit`) and by polkit.
- Steps 17–18:
  - `cmd` actions now run in named `agentgw-run-<hex>.service` units, so the polkit rule can allow only `agentgw-(run|agent)-*` transient units (start/stop) and the `units` allowlist (start).
  - **Residual risk:** polkit cannot see transient-unit properties, so control of the `agentgw` account is root-equivalent (it could request `User=root`). This is documented in the module and the README.
  - The service runs with `NoNewPrivileges=true` (D-Bus plus polkit needs no setuid) and `CapabilityBoundingSet=""`.
  - **The VM test caught a real bug, now fixed:** stopping agentgw left `agentgw-run-*` transient units running (killing the systemd-run client doesn't stop a PID 1-owned unit). On cancellation, runCommand now also asks systemd to stop the named unit (`--no-block`). All five VM subtests pass: health/auth, unsigned webhook 401, sandboxed cmd + allowlisted unit end to end, polkit refusing non-allowlisted units and arbitrary transient units, and no orphaned units after stop.
- Step 19: adding a source takes one new file plus cases in three places (config `validateSource`, `Pipeline.Poll`, schema regeneration), not "one switch case". The docs state the real number. `examples/agentgw.yaml` covers every source type, rule mode and action type, and passes `agentgw validate`.
- Phase 3 review (fresh Opus reviewer; 1 critical, 2 high, 6 medium, 6 low):
  - Coder fixed C1 (agent steps honour the agent's approve), H1 (routine snapshot plus awaited-step id), M3 (retry requeues with run_after, no worker sleep), M4 (no retry on agent steps), L1–L4, and the M2/L6 docs.
  - Codex fixed M2 (http header keys lower-cased), L5 (the stop call has a 10 s bound) and L6 (schema Duration pattern), and folded RunCmdSplit into cmd.go.
- **H2: sandbox redesigned at the owner's direction ("template unit now").** This changes the spec decision "systemd-run transient units".
  - Every sandboxed cmd/agent run is an instance of the Nix-defined `agentgw-action@.service`, whose hardening is fixed in Nix.
  - agentgw writes `<state>/actions/<id>/job.json` (argv, stdin, env, private files, timeout; 0600). The unit gets it as `LoadCredential=job:…`, and the hidden `agentgw exec-job` runs it inside the unit, writing files to its PrivateTmp.
  - systemd writes stdout/stderr to pre-created 0600 files. The exit code comes from `start --wait`; on failure it is `ExecMainStatus` followed by `reset-failed`.
  - polkit allows only start/stop/reset-failed on `^agentgw-action@[0-9a-f]{16}\.service$` and start on the allowlist. Transient units are refused, which removes the root-equivalence.
  - The agent's MCP config and API key travel in the job file, and apiKeyHelper reads the unit-private copy.
  - New module option `maxActionRuntime` (default 2h) as the RuntimeMaxSec ceiling.
  - `sandbox: systemd` now needs the NixOS module.
- My other fixes from that review:
  - **M1:** startup stops `agentgw-action@*` orphans before RequeueRunning.
  - **M5:** credentials and environmentFile are `types.str` with an absolute-and-outside-the-store assertion.
  - **M6:** VM polkit checks assert "Access denied" from the rule, and cover verbs, non-hex names, transient units and a transient unit with the template's name plus `User=root`.
- The VM test now has 6 subtests and all pass, including the sandboxed cmd running as non-root unable to write agentgw's state dir, and the crash case (SIGKILL → restart → orphans stopped before requeue, no duplicate instances).
- Template-unit follow-up review (fresh Opus reviewer; 1 critical, 2 high, 4 medium, 8 low). All fixed:
  - **C1, a real root escalation in the first redesign.** PID 1 opened `StandardOutput=file:` (and `LoadCredential`) paths inside agentgw's writable dir, so a planted symlink could make root write anywhere.
    - Now PID 1 opens nothing there. The unit runs `agentgw exec-job <dir>/<id>` as its DynamicUser, which reads job.json and creates its outputs with `O_EXCL|O_NOFOLLOW`.
    - Run dirs live in `/var/lib/agentgw-actions` (tmpfiles `2710 agentgw:agentgw-io`).
    - Both agentgw.service and the template have `SupplementaryGroups=agentgw-io`. Files get the group by chown, not setgid, because RestrictSUIDSGID forbids setting setgid; a raw `0o2730` is also ignored by Go's `os.Chmod`, and the VM test caught both.
    - A VM subtest plants the symlink as agentgw and asserts the target is unchanged.
  - **H1:** `LimitFSIZE=16M` and `TasksMax` on the template; agentgw reads at most 1 MiB back.
  - **H2:** the orphan stop is blocking, with template `TimeoutStopSec=20s`, followed by reset-failed. The VM test samples peak instances through a SIGKILL and restart with an agent that takes 15 s to stop; peak ≤ 2.
  - **M1:** reset-failed on cancel and orphan paths.
  - **M2:** polkit refuses every other action for agentgw.
  - **M3:** the VM asserts the action can't read the state DB or agentgw's credentials; denials need "Access denied" (or systemd's own fragment refusal for a template-named transient unit); enable/daemon-reload/set-property are checked; `ACTIVE` counts deactivating units.
  - **M4:** README claims corrected.
  - **L2:** signal exits map to 128+n, with ExecMainCode read numerically.
  - **L3:** os.Mkdir for run dirs.
  - **L4:** `--no-ask-password`.
  - **L5:** env keys validated.
  - **L7:** template hardening (ProtectProc, ProcSubset, ProtectKernelLogs, ProtectClock, ProtectHostname, RestrictAddressFamilies, SystemCallFilter).
  - New config `server.actions_dir`, set by the module.
- **Test-quality note:** an edit to the VM test's slow-stop runner silently didn't apply (nixfmt had reflowed the list), so two stop subtests passed without exercising a slow stop. It was caught by timing analysis and fixed. Edits to generated or formatted files now assert that the match exists.
- The VM test has 7 subtests and all pass.
