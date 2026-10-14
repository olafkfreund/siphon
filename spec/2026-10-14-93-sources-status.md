---
status: draft
issue: 93
intent: intent/2026-10-14-93-sources-status.md
---

# Spec: the Sources page shows what each source has actually done

## Design

### Store (`internal/store/query.go`)

`SourceState` gains:

```go
EventAt      *time.Time `json:"event_at,omitempty"`       // a webhook delivery, or the last handled event
LastRejectAt *time.Time `json:"last_reject_at,omitempty"`
LastReject   string     `json:"last_reject,omitempty"`    // a fixed reason, never request content
```

`SourceStates` and `SourceStateOf` select
`last_poll_at, last_error, event_at, last_reject_at, last_reject`. The
columns already exist (`.schema source_state`), so there's no migration.

### Page data (`internal/web/data.go` `sources()`)

`sourceView` gains `LastEventAt *time.Time`, `LastReject string`,
`LastRejectAt *time.Time` and `Check *store.ConnectionCheck`, plus a
`ToolsOnly bool` for an mcp source that isn't polled.

**Webhook sources:**
- `LastEventAt = st.EventAt`;
- health is `ok` once there's a delivery, otherwise `idle`;
- a reject newer than the last delivery shows as a muted warning line
  ("Last refused <ago>: <reason>"), without changing health. A flood of bad
  posts isn't the source's fault.

**MCP sources with `!src.Polled()`:**
- `ToolsOnly = true`;
- `Check` comes from `store.GetConnectionCheck(db, connectionOf(name, src))`,
  where `connectionOf` is `src.Connection`, or the legacy grouping key
  that `connSources` uses (`legacyKey(name)`). It's a small helper beside
  `connSources`, sharing its logic.
- health is `ok` when the check is ok, `error` when it failed (with
  `LastError` = "Connection test failed: <detail>"), and `idle` when there's
  no check yet.

**Everything else** is unchanged: polled sources use `last_poll_at`, and
schedules use `fillSchedule`.

### Template (`internal/web/templates/sources.html`)

The status line becomes:

| Source | Text |
|---|---|
| webhook, delivered | `Last delivery <ago>` |
| webhook, none yet | `Waiting for the first delivery.` |
| tools-only mcp, checked | `Agent tools · connection test <ok|failed> <ago>` (plus the identity detail if ok, such as `siphon.integration`) |
| tools-only mcp, unchecked | `Agent tools · not tested yet` |
| polled, or a schedule | as today |

There's an optional second line for a recent reject on a webhook. There
are no inline styles or scripts (CSP), and it uses the existing classes.

### API

`/api/sources` and `siphon get sources -o json` return the new fields,
since they serialise `sourceView`. In `diagapi.go:233`, the rule diag's
`srcOut` gains `last_event_at` for webhooks.

## Alternatives rejected

- **Writing `last_poll_at` on webhook deliveries.** It would mix "polled"
  and "delivered", and the stale check (`2×poll`) would misfire for
  webhooks, which have no poll interval.
- **Hiding tool-only MCP sources from the Sources page.** They're real
  config items, and their health matters to agents.

## Risks

- **Showing the connection test's `Detail`** on the Sources page: it's the
  identity (a user name) or an error text, both already masked when stored
  (`runConnectionTest` masks before `SetConnectionCheck`).
- **Health for webhooks becomes `ok` after one delivery** and never goes
  stale, because webhooks have no expected cadence. That's the same as the
  Connections page.

## Verification

- **`internal/store`:** `SourceStates` returns `EventAt` and the reject
  after `SetSourceEvent` and a recorded reject.
- **`internal/web`, a sources page test:**
  - a webhook with an event shows "Last delivery" and health `ok`;
  - without one, "Waiting…";
  - a tools-only MCP source with a stored ok check shows "Agent tools ·
    connection test ok" and health `ok`;
  - with a failed check, health is `error`;
  - with none, "not tested yet";
  - a polled http source is unchanged.
- **`TestTemplatesAreCSPClean`;** `go vet ./... && go test -race ./...`.
- **On siphon-dev:** the `test` card shows "Last delivery", and
  `servicenow` shows "Agent tools · connection test ok … siphon.integration".
