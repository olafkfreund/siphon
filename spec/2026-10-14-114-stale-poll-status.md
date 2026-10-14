---
status: draft
issue: 114
intent: intent/2026-10-14-114-stale-poll-status.md
---

# Spec: a source's status shows only real poll failures, and only while it polls

## Design

### 1. Don't store a cancelled poll, in `internal/job/pipeline.go` `tick` (`:201-209`)

**How the two kinds of cancellation differ:**
- **A reload or a shutdown:** `pollLoop` (`internal/job/queue.go:102-124`)
  passes its own `ctx` to `tickAndNudge`, and from there to `tick`. That
  context is cancelled by both, so `ctx.Err() != nil`.
- **A poll's own timeout:** it comes from the source's HTTP client
  timeout, or from the MCP connect timeout inside `poll`. Those fail the
  call with an error, but the context `tick` holds isn't cancelled, so
  `ctx.Err() == nil`.

**The change:**

```go
ev, err := p.poll(ctx, cfg, name)
if err != nil && ctx.Err() != nil {
    return nil, err // cut short by a reload or shutdown: not the source's failure
}
```

That line goes before `PutSourceState`, so the previous status stands.
`tickAndNudge` already skips logging when `ctx.Err() != nil`, so nothing
is logged either.

### 2. Show poll status only for polled sources, in `internal/web/data.go` `sources()` (`:102-104`)

```go
if st, ok := states[name]; ok && src.Polled() {
    v.LastPollAt, v.LastError = st.LastPollAt, st.LastError
}
```

- **Health:** it's derived from `LastError` (`:128`), so a source that
  doesn't poll no longer shows "error" from a stale row.
- **`siphon why`:** it reads the same `sourceView`
  (`internal/web/diagapi.go:233,282`), so its "Source X is failing" goes
  away for sources that don't poll, with no change there.
- **Webhook sources:** they keep their own event and reject fields
  (`:105-110`). Those don't come from poll state.

## Alternatives rejected

- **Deleting the source_state row when a source stops polling.** That
  writes during config apply, and the row also holds webhook event and
  reject state. Hiding it is enough.
- **Telling cancellations apart by the error text** ("context canceled").
  It's fragile. The parent context is the true signal.

## Risks

- **A poll whose own context is cancelled, other than by a reload or
  shutdown,** would also be skipped. No such path exists: `poll` derives
  its contexts from `ctx`, and only the loop cancels that.
- **The CLI's `siphon run --once`** (`pipeline.go:656`) passes a
  background-derived context. If it's interrupted, the error isn't
  stored, which is correct.

## Verification

- **`internal/job`:** with a stub MCP or HTTP source whose poll blocks
  until cancelled, cancelling the context `tick` gets leaves the earlier
  `source_state` (OK) unchanged. A poll that fails on its own, with the
  context not cancelled, still stores its error.
- **`internal/web`:** a source with a stored error that no longer polls
  (an mcp source without `read`) shows no `LastError` and is not
  "error"-health on `/sources` or in `/api/diag/rule`. A polled source
  with an error still shows it.
- **`go vet ./... && go test -race ./...`.**
