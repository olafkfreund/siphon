---
status: draft
issue: 36
intent: intent/2026-10-08-36-notifications.md
---

# Spec: notifications

## Design

**Summary:** a dedicated **notifier** in the daemon. It works out what
needs telling from the database Siphon already writes, keeps each message
in a durable outbox, and delivers it to **channels** (ntfy, Slack, a
generic webhook) over a guarded HTTP client. It touches none of the paths
that create or fail jobs.

### Channels (config)

A new top-level `notify:` map, editable like the other kinds (an overlay
kind `notify`, `apply -f`, history and restore):

```yaml
notify:
  phone:
    type: ntfy                      # ntfy | slack | webhook
    url: file:/run/secrets/ntfy-url # a secret: https://ntfy.sh/<topic>, a Slack hook URL, any https URL
    token: env:NTFY_TOKEN           # optional: ntfy access token, or webhook bearer
    events: [approval, reminder, failed, source]   # default: all four
```

- **`url` is a `Secret`,** for every type: a Slack hook URL is a
  credential, and an ntfy topic is a shared secret. The CLI and the
  portal store it write-only, as a `pendingSecret` file
  (`notify--<name>+url`), like other pasted secrets. It is masked in
  output, errors and the audit log.
- **Validation:** `type` is known; `url` is https, or http only to a
  `server.services.private_endpoints` host; `events` are known names;
  the name follows the usual naming rule.

### What triggers a message

Every 15 s the notifier scans the database, so no hooks are needed in the
four places that fail a job (`FinishJob`, `requeue_failed`, approval
expiry, routine pause). It also catches any future path for free.

| Event | Found as | Dedupe key |
|---|---|---|
| `approval` | a job in `pending_approval` with an open `approvals` row | `job:<id>:<resume_step>` (a routine can pause again at a later step) |
| `reminder` | the same, with ≤ 4 h left of the 24 h `approvalTTL` | `job:<id>:<resume_step>` |
| `failed` | a job `failed` with `finished_at` after the channel's baseline | `job:<id>` |
| `source` | a source whose `failing_since` is older than 15 min; then, once it is healthy again, a matching **recovered** message | `src:<name>:<failing_since>` |

- **New column for `source`:** this needs `source_state.failing_since`.
  `PutSourceState` (`internal/store/store.go:219`) sets it on the first
  error and clears it on success. That's one SQL change, used by both
  `pipeline.go:197` and `schedule.go`.
- **Baseline:** a new channel starts at "now". It never sends history:
  no flood when a channel is first added, or added again later.
- **No catalogue Test checks:** the stored Test results from the catalogue
  are not notified. A person ran the Test and has already seen the result.
- **Webhook-only sources:** a pure webhook source can't "fail" (nothing
  polls it), so it is never reported. The docs say so.

### Outbox and delivery

- **Migration 0005:**
  - `notifications(id, channel, event, key, job_id, source, title, body,
    created_at, attempts, next_at, sent_at, state, error)`, with `UNIQUE(channel,
    event, key)`;
  - `notify_channel(name, since)` for the baselines;
  - `source_state.failing_since`.
- **Recording:** each scan does an `INSERT OR IGNORE`, then delivers rows
  that are due. A restart loses nothing and sends nothing twice. At least
  once is possible if the process dies mid-POST; that's accepted.
- **Retries:** 30 s, 2 m, 10 m, 1 h, 6 h, then the row is `failed`, with an
  audit `notify_failed`. A 4xx other than 408 or 429 fails at once (a bad
  URL or token won't heal).
- **Rate cap:** at most 30 messages per channel per hour. Beyond that,
  rows are `suppressed`, and the next delivered message says "N more
  suppressed".
- **No loops:** a failed notification writes only an outbox row and an
  audit entry. It creates no event, job or rule state.

### Message content

The message is fixed text built from metadata only: the event kind, rule
name, job id, action kind (agent/cmd/routine/unit), source name, and time
left. **It never includes event payloads, prompts or job output.** Those
are untrusted and may be sensitive.

- **Link:** with `server.public_url` set, the message links to
  `<public_url>/jobs/<id>` (or `/sources`). Without it, it gives the CLI
  command (`siphon approve 42`). It never includes an approval token.
- **ntfy:** a POST to the URL with the `Title`, `Priority` (high for
  approval and reminder), `Tags` and `Click` headers, plus
  `Authorization: Bearer` when a token is set.
- **Slack:** `{"text": …}`, with the link as `<url|Open in Siphon>`.
- **webhook:** JSON `{"event","rule","job","source","title","message","url","at"}`,
  plus the bearer token when set. The schema is documented, for people who
  forward it elsewhere.

### Network

- **Guarded client:** delivery goes through the guarded-client pattern
  that already exists (`draft.GuardedClient`, `source.ResolveAllowedMode`).
  It allows public addresses only, unless the exact host:port is in
  `server.services.private_endpoints` (never link-local). It dials the
  checked address, follows no redirects, and times out after 10 s.
- **Where it runs:** the daemon itself sends, not a sandboxed unit. So it
  works the same with `sandbox: none` (the OCI image).

### CLI, API, portal

- **CLI:**
  - `siphon notify add <name> --type ntfy|slack|webhook --url - [--token -] [--events a,b]`:
    secrets come from stdin or `@file` only, with the usual dry-run diff
    and `--yes`;
  - `siphon notify test <name>`: sends a test message now, and reports the
    status;
  - `siphon notify log [--channel] [-o json]`: recent deliveries;
  - `siphon get notify` and `siphon delete notify <name>`, through the
    existing generic paths.
- **API:**
  - the existing config API, for kind `notify`;
  - `POST /api/notify/{name}/test`;
  - `GET /api/notifications`.
- **Portal:** a **Notifications** page.
  - **Channels:** each channel shows its last delivery and a **Test**
    button.
  - **Add:** a form with three type tiles, the URL and token as
    write-only fields, and event checkboxes.
  - **Deliveries:** a recent-deliveries table.
  - **Job page:** each job's page lists the notifications sent for it.
- **`siphon mcp`:** gains `notify_test` and the `notify` kind. Writes stay
  gated as today, and the URL and token need `--allow-secrets`.

### Docs

- `docs/tasks/notifications.md`: set up ntfy on a phone, Slack, and a
  webhook; test; troubleshooting.
- Entries in `docs/llm.md` and `AGENTS.md`, plus the regenerated
  `docs/cli.md` and `llms-full.txt`.
- The `docs/getting-started.md` checklist gains "get told when an agent
  needs approval".

## Alternatives rejected

- **A built-in `siphon` event source, with rules sending the messages:**
  it would reuse rules and actions. But each notification would be a
  sandboxed job with its own egress allowlist. A Slack URL would have to
  reach the command's argv or environment. A notification job that fails
  emits the very event it reports, so it needs a loop guard. And "one
  command to set up" becomes a source, a rule and a credential. The
  dedicated notifier is smaller and can't loop.
- **Hooks in each write path (an outbox row in the same transaction):**
  it's lower latency, but touches `FinishJob`, `RequeueRunning`,
  `ExpireApprovals`, routine pause and `PutSourceState`, and every
  future failure path has to remember it. A 15 s scan is good enough for
  messages that wait on a human.
- **Email (SMTP) in v1:** deferred by the owner. ntfy and the webhook can
  forward to email.
- **Per-rule routing:** deferred by the owner. Events are routed by kind
  per channel.
- **A one-click approve link in the message:** rejected, because a message
  is not an authenticated channel (the intent's constraint).

## Risks

- **Polling cost:** a scan every 15 s. Each query is indexed (job state
  and `finished_at`, the approvals join, and `source_state` is tiny), so
  the cost is negligible at Siphon's size.
- **Duplicate on crash:** a crash between a successful POST and the
  `sent_at` write sends the message twice. That's accepted, and documented
  as at-least-once.
- **Secret leakage through errors:** the guarded client's errors include
  the URL. Errors are masked with the existing masker before they are
  stored or logged, and a test pins this.
- **Noise from flapping sources:** the 15-minute threshold, one message
  per failing episode, and the hourly cap bound it.
- **Hosts:** none changed. The NixOS module needs no new option (the
  config passes through), and the daemon already makes outbound requests.

## Verification

- **Unit tests** (with an injected clock):
  - each event kind is produced once;
  - the baseline means no history;
  - the routine re-pause key;
  - the reminder window;
  - the failing threshold and recovery;
  - the backoff schedule, and 4xx versus 5xx;
  - the cap with the suppressed count;
  - masking of the URL and token in errors;
  - the guard refuses private addresses unless listed;
  - each channel's request format, against `httptest`.
- **Config:** validation and overlay round-trip, the schema regenerated,
  and CSP-clean templates.
- **VM subtest:**
  - a local HTTP receiver is set up as a `webhook` channel, and the
    `private_endpoints` entry is covered;
  - an agent job that needs approval → one delivery;
  - a failing `cmd` → one delivery;
  - a restart sends nothing twice;
  - `siphon notify test` → a delivery;
  - `siphon notify log -o json` shows them.
- **CI:** the usual (`go vet`, race tests, `nix flake check`, the OCI
  test), plus a fresh Opus security review before the PR.
- **Owner live check:** add an ntfy channel for your phone, then trigger
  an approval and get the message.
