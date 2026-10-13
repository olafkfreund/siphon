---
status: draft
issue: 65
intent: intent/2026-10-13-65-test-last-headers.md
---

# Spec: siphon test --last and why replay the event's headers

## Design

**What gets stored.** `deliver` (`internal/job/hooks.go:103-104`) stores the
last event with `ev.Headers`. Those headers are already what the rules saw:
the webhook handler (`internal/source/webhook.go:225-240`) drops the
configured signature or token header, `x-hub-signature-256`,
`authorization`, `cookie`, the proxy-auth headers and the hop-by-hop
headers before building them.

As a second layer, `store` drops any header whose name matches
`secretKey` (`overlay.go:117`: token, secret, password, authorization,
api_key, cookie) or a new `secretHeader` pattern: `signature`, `-sig$`,
`-key$`, `auth`. The full name list is in the plan. So a token header that
isn't the source's configured one is never kept either, for example a
second `x-siphon-key`.

The stored headers are capped at 8 KiB. Above that, it stores `{}`, as the
64 KiB event cap already does.

**Where.**
- **Migration `0006_event_headers.sql`:** `ALTER TABLE source_state ADD
  COLUMN headers TEXT NOT NULL DEFAULT '{}'`. Old rows read as no headers,
  which is today's behaviour.
- **`store.SetSourceEvent(db, source, data, headers map[string]string)`:**
  it gains the argument. `schedule.go:152` and `pipeline.go:207` pass
  `nil`, so polled and scheduled sources are unchanged.
- **`store.SourceEventHeaders(db, source) (map[string]string, error)`:**
  new. `SourceEvent` keeps its signature, so its five other callers don't
  change.

**Who replays them.**
- `POST /api/rules/{name}/test` with `use_last` (`diagapi.go:45-52`) starts
  from the stored headers. Headers given in the request still override
  them, key by key.
- `explain`, the API behind `siphon why` (`diagapi.go:188`, `:248`), passes
  the stored headers to `ruleTest` instead of `nil`.
- The portal's rule tester (`editors.go:193`) does the same.
- `GET /api/sources/{name}/last-event` (`diagapi.go:69-85`) returns
  `"headers"` next to `"event"`.

## Alternatives rejected

- **An allowlist of event-type headers:** decided against at intent
  approval. Rules can read any header.
- **Store the headers inside the event JSON (`_headers`):** it would
  change what `--last` and the portal show as the event, and `RedactKeys`
  would walk them as data.
- **Change `SourceEvent` to return both values:** it touches six callers
  for no gain.

## Risks

- **A secret in a header we don't recognise:** for example a vendor that
  sends its shared secret in `x-foo-shared`, and the user doesn't
  configure it as `token_header`. That header was already visible to rules
  and to the portal's test page. Storing it adds persistence in the
  SQLite DB, which already holds rule state and audit. The denylist covers
  every vendor header the catalog knows, and the plan lists them with a
  test for each.
- **Migration on an existing DB:** an additive column with a default, the
  same as `0003`. `siphon backup` and restore carry it automatically.

## Verification

- **Store test:**
  - `SetSourceEvent` with headers that include `x-github-event`,
    `x-siphon-key`, `stripe-signature`, `x-gitlab-token` and
    `authorization` stores only `x-github-event`;
  - an over-cap set stores `{}`;
  - an old row with no column value reads as empty.
- **API test** (`internal/web`):
  - a webhook for a rule with `when: headers["x-github-event"] == "push"`
    goes through the real handler;
  - then `POST /api/rules/r/test {"use_last": true}` reports a fire;
  - `explain` reports `last_event_matches: true`;
  - `last-event` shows the header, and no signature header.
- **Unchanged behaviour:** existing `diagapi` and `editors` tests still
  pass, and so do polled and scheduled sources.
- `go vet ./... && go test -race ./...`.
- **After merge:** the #60 demo can bring back `siphon why ci-failed` and
  `siphon test ci-failed --last`. That's a separate small change on that
  PR, or a follow-up if #60 merges first.
