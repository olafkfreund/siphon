---
status: approved
issue: 65
spec: spec/2026-10-13-65-test-last-headers.md
---

# Plan: siphon test --last and why replay the event's headers

## Approved decisions

- The last event of a webhook source is stored with its headers. These are
  `ev.Headers`, exactly what the rules saw: the webhook handler
  (`internal/source/webhook.go:225-240`) already drops the configured
  signature or token header, `x-hub-signature-256`, `authorization`,
  `cookie`, the proxy-auth headers and the hop-by-hop headers.
- **Second layer, in `store`:** drop any header whose lower-cased name
  matches `secretKey` (`internal/store/overlay.go:117`) or
  `secretHeader = (?i)signature|-sig$|auth|(^|-)(api|access|secret|private|siphon)-key$`.
  - **Dropped:** `x-siphon-key`, `x-api-key`, `stripe-signature`,
    `x-gitlab-token`, `cf-webhook-auth`, `authorization`,
    `x-hub-signature`, `webhook-signature`.
  - **Kept:** `x-github-event`, `x-event-key` (Bitbucket),
    `x-gitea-event`, `x-forgejo-event`, `x-gitlab-event`, `content-type`,
    `user-agent`, `x-github-delivery`.
  - The pattern deliberately doesn't match a bare `-key$`, because that
    would drop Bitbucket's `x-event-key`.
- Stored headers over 8 KiB, as JSON, are stored as `{}`.
- **The column:** a new migration `0006_event_headers.sql` adds `headers TEXT
  NOT NULL DEFAULT '{}'` to `source_state`. Old rows read as empty.
- `SetSourceEvent(db, source, data, headers map[string]string)` gains the
  argument. Polled and scheduled sources pass `nil`.
- `SourceEventHeaders(db, source) (map[string]string, error)` is new.
  `SourceEvent` doesn't change.
- **Replayed by:**
  - `use_last` in `POST /api/rules/{name}/test`, where request headers
    override the stored ones key by key;
  - `explain` (`siphon why`);
  - the portal's rule tester, where form headers override likewise;
  - `GET /api/sources/{name}/last-event`, which returns `"headers"`.

## Steps

1. **`internal/store`.**
   - Add `migrations/0006_event_headers.sql`.
   - In `overlay.go:143-170`:
     - add the `secretHeader` regexp next to `secretKey` (line 117);
     - `SetSourceEvent` takes `headers`, filters them, caps them at 8 KiB,
       and writes them in the same `INSERT … ON CONFLICT` (also
       `headers=excluded.headers`);
     - add `SourceEventHeaders`, which returns an empty map for no row or
       `'{}'`.
   - Update the test callers to pass `nil`:
     - `store_test.go:271,282`;
     - `internal/web/editors_test.go`, `diagapi_test.go`, `schedule_test.go`;
     - `internal/job/schedule_test.go`.
   - Add `TestSourceEventHeaders`:
     - the kept and dropped lists above, exactly;
     - an over-cap set stores `{}`;
     - a row written before the column existed (`UPDATE … SET
       headers='{}'`) reads as empty.
   - Verify: `go test -race ./internal/store/`.
   - Traps: never log header values. The migration must be additive only,
     and it runs on existing DBs at start.

2. **Callers that store.**
   - `internal/job/hooks.go:104` passes `ev.Headers`.
   - `internal/job/schedule.go:152` and `internal/job/pipeline.go:207`
     pass `nil`.
   - Verify: `go build ./... && go test -race ./internal/job/`.
   - Traps: none.

3. **`internal/web/diagapi.go`.**
   - **The `use_last` branch (lines 45-52):** load `SourceEventHeaders`
     into `headers` before the request's `body.Headers` are copied in
     (lines 63-66), so the request wins.
   - **`explain` (line 188 and line 248):** load the headers next to `raw`,
     and pass them to `ruleTest` instead of `nil`.
   - **`last-event` (lines 69-85):** add `"headers": h` to the response.
   - Verify: `go test -race ./internal/web/ -run 'RuleTest|Explain'`.
   - Traps: errors from `SourceEventHeaders` return 500 with "internal
     error", as the neighbours do. Don't put a value in an error.

4. **`internal/web/editors.go:193-205`, the portal tester.** When it uses
   the stored event, start `headers` from `SourceEventHeaders`, then apply
   the `headers_json` form values over them (lines 181-190).
   - Verify: `go test -race ./internal/web/ -run Editor`.
   - Traps: CSP-strict templates, so no template change is needed.

5. **The end-to-end API test, in `internal/web/diagapi_test.go`**, next to
   `TestRuleTestAndLastEventAPI` (line 147).
   - Set up a webhook source with `signature: github`, a known secret, and
     a rule `when: 'headers["x-github-event"] == "push"'`.
   - POST a signed push through the real `/hook/<source>` handler, with
     `X-GitHub-Event: push` and also `X-Siphon-Key: s-LEAK`.
   - Then check:
     - `use_last` reports a fire;
     - `explain` has `last_event_matches: true`;
     - `last-event` has `"x-github-event":"push"`;
     - no response contains `s-LEAK` or `sha256=`.
   - Verify: `go test -race ./internal/web/ -run LastEventHeaders`.
   - Traps: inject the clock if the handler checks timestamps (`github`
     doesn't). Socket paths ≤107 bytes if a test needs one.

6. **The full run, and the docs.**
   - In `docs/troubleshooting.md`, if it says `--last` ignores headers,
     correct it (grep for "--last").
   - Then `go generate ./docs`, `go vet ./...` and `go test -race ./...`.
   - Verify: green, and `git diff --exit-code llms-full.txt` after
     generate.
   - Traps: `llms-full.txt` is generated.

## Tests

```sh
go vet ./... && go test -race ./...
go generate ./docs && git diff --exit-code llms-full.txt
```

## Rollback

Revert the PR. The `0006` column stays in existing DBs, harmless and
ignored by the old code. A down-migration isn't needed, since nothing
reads it once reverted.
