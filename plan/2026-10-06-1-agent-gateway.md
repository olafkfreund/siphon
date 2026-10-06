---
status: draft
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

## Handoff and review

- There are 19 file-editing steps, so the `coder` agent (Sonnet) implements them. Start it with this plan path and step 1, and send each later step to the same agent with `SendMessage`.
- After each phase, a fresh Opus reviewer gets only this plan path and `git diff` for that phase.
- One PR per phase into `main`, each linking intent, spec and plan, and stating which steps the coder did.
- Any deviation updates this file in the same commit as the code.

## Rollback

- Greenfield repo; nothing is deployed. To roll back a step, `git revert` its commit; each step is one commit.
- No host is changed by this plan. Deploying the NixOS module to a real host is a separate, approved change; it is removed by disabling `services.agentgw.enable` and rebuilding. The state is `/var/lib/agentgw` and can be deleted.
