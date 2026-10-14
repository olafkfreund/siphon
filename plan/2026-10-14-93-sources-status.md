---
status: approved
issue: 93
spec: spec/2026-10-14-93-sources-status.md
---

# Plan: the Sources page shows what each source has actually done

## Approved decisions

- **`store.SourceState`** gains `EventAt`, `LastRejectAt` and `LastReject`,
  read from existing `source_state` columns. There's no migration.
- **Webhook cards:** `Last delivery <ago>` from `event_at`, health `ok`
  once delivered, otherwise `idle`. A reject newer than the last delivery
  shows as a muted line; health doesn't change.
- **Tool-only MCP cards** (`type: mcp`, `!Polled()`):
  - `ToolsOnly`;
  - the connection check comes from `GetConnectionCheck` under
    `src.Connection`, or the legacy key (`legacyKey(name)`) that
    `connSources` uses;
  - health is `ok`, `error` (with `LastError` "Connection test failed:
    <detail>") or `idle`;
  - the text reads `Agent tools · connection test ok|failed <ago>` (plus
    the detail when ok), or `Agent tools · not tested yet`.
- **Polled sources and schedules** are unchanged.
- **The API** serialises the same `sourceView`. The rule diag's `srcOut`
  gains `last_event_at`.

## Steps

1. **The store** (`internal/store/query.go:13-40`, and `SourceStateOf` at
   about `:205`).
   - Add the three fields, and select
     `last_poll_at, last_error, event_at, last_reject_at, last_reject` in
     both queries. Use `sql.NullInt64` for the times, as `last_poll_at`
     does.
   - **Test** in `internal/store`: after `SetSourceEvent` and a recorded
     reject (find the writer: grep `last_reject` in `internal/store`),
     `SourceStates` returns both.
   - **Verify:** `go test ./internal/store`.
   - **Traps:** the times are stored in milliseconds (`UnixMilli`).

2. **Page data and template** (`internal/web/data.go`, `sources.html`,
   `diagapi.go:233`).
   - **`sourceView`** gains `LastEventAt`, `LastReject`, `LastRejectAt`,
     `ToolsOnly` and `Check *store.ConnectionCheck` (JSON tags with
     `omitempty`).
   - **`sources()`:** fill them per the decisions. Add the helper
     `connectionOf(name string, src *config.Source) string` next to
     `connSources` (`connection_test_run.go:27`), reusing its legacy rule.
     Compute health as the decisions say.
   - **The template line** (`sources.html:6`): the table from the spec,
     plus an optional reject line. Use the existing classes (`muted`,
     `err`, `small`) and `agop`. No inline style or script.
   - **`diagapi.go:233`:** add `srcOut["last_event_at"]`.
   - **Tests** in `internal/web`, a new `sources_status_test.go`, using the
     existing server test helpers. Cover:
     - a webhook with an event shows "Last delivery" and health `ok`;
     - a webhook without one shows "Waiting…";
     - a webhook with a reject newer than its delivery shows the reject
       line;
     - a tools-only MCP source with an ok check, with a failed check
       (health `error`), and with none ("not tested yet");
     - a polled http source is unchanged.
   - **Verify:** `go test -race ./internal/web`. `TestTemplatesAreCSPClean`
     must pass.
   - **Traps:** `GetConnectionCheck` returns nil with no error when there's
     no row. Check that, and don't treat it as a failure.

3. **All checks, and siphon-dev (mine).**
   - `go vet ./... && go test -race ./...`.
   - Build, swap into siphon-dev (keeping a `.prev`), restart by PID, then
     look at the Sources page:
     - `test` shows "Last delivery";
     - `servicenow` shows "Agent tools · connection test ok … siphon.integration".

## Tests

```sh
go test ./internal/store
go test -race ./internal/web
go vet ./... && go test -race ./...
```

## Rollback

Revert the PR. It's read-only display code, with no data change.

## Implementation

Steps 1–2 touch four files plus tests, so a `coder` agent does them and a
fresh Opus agent reviews them. Step 3 is mine.

## Deviations

- **Step 2:** a new template function, `agot`, takes a `time.Time` value.
  `agop` takes a pointer, and `ConnectionCheck.At` is a value. A failed
  check shows as the error line "Connection test failed: <detail>", with no
  separate time.
