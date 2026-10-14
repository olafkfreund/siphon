---
status: approved
issue: 114
spec: spec/2026-10-14-114-stale-poll-status.md
---

# Plan: a source's status shows only real poll failures, and only while it polls

## Approved decisions, carried over

- **A poll cut short by a reload or a shutdown** (`ctx.Err() != nil` on the
  context `tick` received from `pollLoop`) isn't stored. `tick` returns
  before `PutSourceState`, and the previous status stands.
- **A poll's own timeout** (the client or connect timeout, with the
  context not cancelled) is stored, as today.
- **The Sources page** sets `LastPollAt` and `LastError` only for
  `src.Polled()` sources. Health and `siphon why` (`diagapi.go:233,282`)
  follow from that.
- **No migration, and no deleted rows.**

## Steps

1. **`internal/job/pipeline.go` `tick` (`:201-209`).**
   - After `ev, err := p.poll(ctx, cfg, name)`, add
     `if err != nil && ctx.Err() != nil { return nil, err }` with a
     one-line comment.
   - **The tests, in `internal/job`** (reuse `newPipeline` in
     `queue_test.go:13` and an MCP or HTTP stub):
     - store an OK state first, then a poll that blocks until its context
       is cancelled, then cancel it: the source state is unchanged;
     - a poll that fails on its own (a stub returning 500) with a live
       context: the error is stored.

   → verify: `go test -race ./internal/job`.

   Traps: no real sleeps. Cancel the context explicitly, or block on a
   channel.

2. **`internal/web/data.go` `sources()` (`:102-104`).**
   - Guard with `&& src.Polled()`.
   - **The test, in `internal/web`:** a config with an mcp source without
     `read` (not polled) and a polled http source, both with a stored
     `last_error`:
     - `/sources` shows the polled one's error and not the other's;
     - the diag API (`/api/diag/rule/<r>` for a rule on the unpolled
       source) has no `source_error` reason.

   → verify: `go test -race ./internal/web`.

## Deviations

- **Step 2: the guard is `src.Polled() || src.Type == "schedule"`.** The
  scheduler records a schedule source's last run in `LastPollAt`, and
  `Polled()` is false for schedules. `TestScheduleSourceView` caught it.
- **The diag check** uses `/api/rules/<r>/explain`, the path
  `diagapi_test.go` already uses.

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** 2 steps over 4 files, under the coder threshold for steps
but at it for files. The coder does both steps, as one handoff. A fresh
Opus agent reviews the diff.

## Rollback

Revert the PR. No stored state changes.
