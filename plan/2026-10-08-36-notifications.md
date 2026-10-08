---
status: approved
issue: 36
spec: spec/2026-10-08-36-notifications.md
---

# Plan: notifications

You can implement this plan without opening the intent or the spec.

## Approved decisions (from the spec)

- **D1, dedicated notifier:**
  - It is a dedicated notifier in the daemon, not a built-in event source
    with rules.
  - It scans the database every **15 s**.
  - It adds **no hooks** in `FinishJob`, `RequeueRunning`,
    `ExpireApprovals`, routine pause or anywhere else that fails a job.
- **D2, channels:** a top-level `notify:` map, `name → {type, url, token, events}`.
  - `type` is `ntfy | slack | webhook`.
  - `url` is a `Secret` for **every** type. `token` is an optional
    `Secret` (an ntfy access token, or a webhook bearer).
  - `events` is a subset of `approval, reminder, failed, source`, and
    defaults to all four.
  - It is an overlay kind `notify`. Pasted secrets go in `pendingSecret`
    files named `notify--<name>+url` and `+token`.
  - `url` must be https, or http only to a host:port listed in
    `server.services.private_endpoints`.
- **D3, events and dedupe keys:**

  | event | found as | key |
  |---|---|---|
  | `approval` | the job is `pending_approval` with an open `approvals` row (`decision IS NULL`) | `job:<id>:<resume_step>` |
  | `reminder` | the same, with `expires_at - now ≤ 4h` (the TTL is 24 h, `approvalTTL` in `internal/job/approval.go:15`) | `job:<id>:<resume_step>` |
  | `failed` | `state='failed'` and `finished_at > ` the channel's baseline | `job:<id>` |
  | `source` | `failing_since` is older than **15 min** → a "failing" message; once it is healthy again → one **recovered** message, sent only if a failing one was sent | `src:<name>:<failing_since ms>` (recovered uses event `source_ok`, same key) |
- **D4, baseline:** a channel's first scan stores `notify_channel(name, since=now)`.
  Nothing from before the baseline is ever sent. Deleting the channel
  deletes its baseline row, so adding it again starts fresh.
- **D5, outbox:** the `notifications` table has `UNIQUE(channel, event, key)`.
  - **Recording:** `INSERT OR IGNORE`.
  - **Retries:** 30 s, 2 m, 10 m, 1 h, 6 h, then `failed`, with an
    audit `notify_failed`. A 4xx other than 408 or 429 → `failed` at once.
  - **Duplicates:** at least once on a crash, which is accepted.
- **D6, rate cap:** at most **30 delivered per channel per rolling hour**.
  Rows over the cap → `suppressed`. The next delivered message on that
  channel appends "(N more suppressed)" and resets the count.
- **D7, no loops:** delivery writes only the outbox and the audit log. It
  creates no event, job or rule state.
- **D8, content:** fixed text built from metadata only.
  - **Included:** the event, rule, job id, action kind, source name and
    time left.
  - **Never included:** an event payload, a prompt, job output or a token.
  - **Link:** `<server.public_url>/jobs/<id>` or `/sources` when
    `public_url` is set; otherwise the CLI hint (`siphon approve <id>`).
  - **ntfy:** a POST with the `Title`, `Priority` (`high` for approval and
    reminder, else `default`), `Tags` and `Click` headers, plus
    `Authorization: Bearer` when a token is set.
  - **Slack:** `{"text": "*<title>*\n<message> <url|Open in Siphon>"}`.
  - **webhook:** `{"event","rule","job","source","title","message","url","at"}`
    (RFC 3339), plus the bearer token when set.
- **D9, network:** the daemon sends directly; there is no sandboxed unit.
  - **Addresses:** public addresses only, unless
    `cfg.ServiceEndpoint(url)` is true (then private, never link-local).
  - **Dialing:** it dials the resolved, checked address and follows no
    redirects.
  - **Limits:** a 10 s timeout, and at most 4 KiB of the response is read.
  - **Errors:** stored and logged errors are masked with `action.Mask`
    against the url and the token.
- **D10, surfaces:**
  - **CLI:** `siphon notify add|test|log`. Secrets come only from stdin
    (`-`) or `@file`, with the usual dry-run diff and `--yes`. `get` and
    `delete` work through the generic kinds.
  - **API:** `POST /api/notify/{name}/test` and `GET /api/notifications`.
  - **Portal:** a **Notifications** page (channels with their last result
    and **Test**, an add form, recent deliveries), plus a section on each
    job's page.
  - **MCP:** a `notify_test` tool, and the `notify` kind, gated like the
    others. The url and token need `--allow-secrets`.
- **Out of scope:** email, per-rule routing, one-click approve links, and
  notifying the catalogue's Test results.

## Steps

1. **Config kind `notify`.**
   - **Files:**
     - `internal/config/config.go`: a `Notify` struct (`Type string`,
       `URL Secret`, `Token Secret`, `Events []string`), and
       `Config.Notify map[string]*Notify yaml:"notify"`, after
       `Credentials` (l.91).
     - In `Validate`, a `validateNotify`: a known type, a set url,
       events in the known set, and the name rule like the other kinds.
       The resolved url must parse, and be https or
       `ServiceEndpoint(url)`. The resolution check runs only where
       secrets are resolved, the same way as for other Secrets.
       Secret resolution covers `Notify.URL` and `.Token` (where
       `resolve(stub)` is called for credentials).
     - `internal/config/overlay.go`: add `"notify": true` to `Kinds`
       (l.42), a `checkOverlay` case (nothing operator-only, but
       private-endpoint http is allowed only as above), and `itemRefs`
       for `url` and `token` (l.367).
     - `cmd/siphon/verbs.go:26`: add `configKinds`.
     - `internal/web/data.go:184-186`: add to the `kindTitle`, `kindOne`
       and `kindNav` maps (title "Notifications", one "notification
       channel", nav "notify").
     - `internal/config/explain.go`: a `notify` entry.
     - The schema, regenerated.
   - **Verify:** `go test ./internal/config ./cmd/siphon`, plus new tests
     for validation, overlay round-trip and secret refs. Then
     `UPDATE_SCHEMA=1 go test ./internal/config`.
   - **Traps:**
     - `SecretFileName` injectivity: kinds have no `-`, and `notify`
       complies.
     - `KnownFields` decoding rejects unknown keys. Keep the yaml tags
       exact.

2. **Store: migration 0005 and the queries.**
   - **Migration:** `internal/store/migrations/0005_notifications.sql`.
     - `notifications(id INTEGER PRIMARY KEY, channel TEXT NOT NULL, event TEXT NOT NULL, key TEXT NOT NULL, job_id INTEGER, source TEXT NOT NULL DEFAULT '', title TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL, sent_at INTEGER, state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sent','failed','suppressed')), error TEXT NOT NULL DEFAULT '', UNIQUE(channel,event,key))`,
       with indexes `(state,next_at)` and `(job_id)`;
     - `notify_channel(name TEXT PRIMARY KEY, since INTEGER NOT NULL)`;
     - `ALTER TABLE source_state ADD COLUMN failing_since INTEGER`.
   - **`PutSourceState`** (`internal/store/store.go:219`): on conflict,
     `failing_since = CASE WHEN excluded.last_error='' THEN NULL ELSE COALESCE(source_state.failing_since, excluded.last_poll_at) END`.
     The insert sets it to `now` when `pollErr != ""`.
   - **New queries** in `internal/store/notify.go`:
     - `ChannelSince(name, now)`: get or create;
     - `DropChannel(name)`;
     - `PendingApprovalsFor(since)`: id, rule, resume_step, expires_at,
       action kind;
     - `FailedJobsSince(since)`;
     - `FailingSources()`: name, failing_since;
     - `SentSourceEpisodes(channel)`: source-failing rows that were sent,
       for the recovered message;
     - `EnqueueNotification(...)`: `INSERT OR IGNORE`;
     - `DueNotifications(now, limit)`;
     - `MarkSent`, `MarkRetry`, `MarkFailed` (the last writes audit
       `notify_failed`), `MarkSuppressed`;
     - `SentInLastHour(channel, now)`;
     - `ListNotifications(filter{channel, job, limit})`.
   - **Verify:** `go test ./internal/store`, with new tests for the
     `failing_since` transitions, the unique dedupe, due ordering and the
     migration on an existing 0004 database.
   - **Traps:**
     - Migrations are append-only.
     - Times are ms (`ms(now)`).
     - Find the action kind from the job row the same way
       `dashboard.go` `jobActions` does.

3. **The `internal/notify` package and wiring it into the daemon.**
   - **`notify.go`:**
     - `type Notifier struct{ Store *store.Store; Config func() *config.Config; Now func() time.Time; Client func(cfg, url) (*http.Client, func(), error) }`;
     - `Run(ctx)`, a 15 s ticker that calls `Scan`, then `Deliver`;
     - `Scan` applies D3 and D4 for each channel and each of its events;
     - `Deliver` applies D5, D6 and D8 to the due rows, at most 50 per
       tick.
   - **`format.go`:** builds the per-type request (D8).
   - **`client.go`:** the D9 guard, modelled on `draft.GuardedClient`
     (`internal/draft/draft.go:333`), but with
     `source.Public`/`source.PrivateNoLinkLocal` chosen by
     `cfg.ServiceEndpoint(url)`.
   - **`Send(ctx, cfg, name, n)`:** a direct send used by
     `notify test`. It bypasses the outbox and records nothing except an
     audit `notify_test`.
   - **Wiring:** spawn it from `internal/job/queue.go` `Run`, next to
     `expiryLoop` (l.72): `spawn(func() { notify.New(...).Run(ctx) })`.
     If an import cycle appears, construct it in `cmd/siphon/main.go`
     serve and pass it in.
   - **Verify:** `go test -race ./internal/notify`. Tests use an injected
     clock and `httptest`, and cover:
     - each event once;
     - the baseline (no history);
     - the routine re-pause key;
     - the reminder window;
     - the 15 m threshold and recovery;
     - the backoff sequence;
     - 4xx versus 5xx, 408 and 429;
     - the cap and the suppressed suffix;
     - masked errors (the URL and token never appear);
     - a private address refused, and allowed when listed;
     - the three request formats;
     - no redirect followed.
   - **Traps:**
     - Inject the clock (real timers flake in CI).
     - `httptest` listens on 127.0.0.1, so tests must list it in
       `private_endpoints` or inject `Client`.
     - Never log the url or the token.

4. **API, CLI and MCP.**
   - **API:** `internal/web/api.go`, `post("/api/notify/{name}/test")`
     and `get("/api/notifications")` (query `channel`, `job`, `limit`).
     `internal/client`: matching methods.
   - **CLI:** `cmd/siphon`, a `notify` command group.
     - `add`: builds the item YAML with `url: <pending>`, sends the
       secrets as pendingSecrets through the config API path (like
       `siphon connect … --token -`), and shows the dry-run diff before
       `--yes`.
     - `test`: reports the HTTP status, or the masked error, and exits 1
       on failure.
     - `log`: a table, or `-o json`.
     - Register it in `help --json`.
   - **MCP:** `cmd/siphon/mcp.go`, a `notify_test` tool, plus the `notify`
     kind in the existing write tools. The secret fields need
     `--allow-secrets`.
   - **Verify:** `go test ./internal/web ./cmd/siphon ./internal/client`,
     and `go generate ./docs` (the `docs/cli.md` and `llms-full.txt`
     freshness tests).
   - **Traps:**
     - `-o json` is not consent: `add` needs `--yes`.
     - Exit codes 0 to 5.
     - Secrets never come from a flag value.

5. **Portal.**
   - **Files:**
     - `internal/web/templates/notify.html`: a channel list (name, type,
       events, last delivery state and time, a **Test** button posting
       through htmx), an add form (three type tiles, url and token as
       write-only password fields, event checkboxes), and a table of the
       last 50 deliveries;
     - a route in `portal.go`;
     - the add, delete and test handlers, committed through
       `s.commit(..., pendingSecret{Kind:"notify",…})` as in
       `internal/web/models.go:73`;
     - the nav link in `layout.html`, after Egress, with an existing
       sprite icon (`bell` if present, otherwise add a `bell` symbol to
       `sprite.html`);
     - on `job.html`, a "Notifications" section listing the job's rows.
   - **Verify:** `go test ./internal/web` (`TestTemplatesAreCSPClean`),
     plus light and dark screenshots on a copy of the dev data.
   - **Traps:**
     - No inline `style=` or `<script>`.
     - Secret inputs are never echoed back.

6. **Docs.**
   - **Files:**
     - `docs/tasks/notifications.md`: ntfy to a phone, Slack, a webhook
       and its JSON schema, `notify test`, the events table, the limits
       (webhook-only sources, at least once), and troubleshooting;
     - links from `docs/README.md` and the `docs/getting-started.md`
       checklist ("get told when an agent needs approval");
     - `docs/llm.md`, `AGENTS.md` and `llms.txt`;
     - the regenerated `llms-full.txt`.
   - **Verify:** `go test ./docs` (links, `llms.txt` and freshness).

7. **VM subtest** in `nix/vm-test.nix`.
   - **Setup:** a Python HTTP receiver on the machine at
     `127.0.0.1:18099` that appends each request body to a file;
     `server.services.private_endpoints` gains `127.0.0.1:18099`; and a
     `siphon notify add hook --type webhook --url - --yes` with
     `http://127.0.0.1:18099/n` on stdin.
   - **Checks:**
     - `siphon notify test hook` → one body in the file;
     - an agent job that needs approval → one `"event":"approval"` within
       30 s;
     - a `cmd` that exits 1 → one `"event":"failed"`;
     - `systemctl restart siphon`, wait 30 s → no new lines;
     - `siphon notify log -o json` lists them as `sent`;
     - delete the channel at the end, so it can't affect later subtests.
   - **Verify:** `nix build -L .#checks.x86_64-linux.vm`.
   - **Traps:**
     - Use PIDs, not `pkill -f`.
     - Count deltas, not totals.
     - Put the subtest before the orphan-count subtests, and clean up.

8. **Finish.**
   - `go vet ./... && go test -race ./...`, `nix flake check`, and the OCI
     test (`ci/container-test.sh`).
   - A fresh Opus security review, given the plan and `git diff main`.
     Fix its findings.
   - Open the PR, linking the intent, spec and plan.

**Hand-off:** steps 1–5 go to the `coder` agent, step by step. Opus does
steps 6–8, and reviews.

## Tests

- `go vet ./... && go test -race ./...`: all pass, including the new
  config, store, notify, web and CLI tests.
- `go test ./docs`: the links and generated files are fresh.
- `nix flake check`: the VM test passes, including the new subtest.
- `bash ci/container-test.sh`: passes (the notifier runs under
  `sandbox: none`).

## Rollback

- Revert the merge commit.
- Migration 0005 only adds tables and a nullable column, so an older
  binary ignores them. The `notify:` items in the overlay would then fail
  `KnownFields` on an older binary. Delete them first
  (`siphon delete notify <name>`), or restore an earlier config revision.

## Deviations log

(none yet)
