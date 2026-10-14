---
status: approved
issue: 121
spec: spec/2026-10-14-121-notify-unpolled-recovery.md
---

# Plan: a source that stops polling ends its "Source failing" episode

## Approved decisions, carried over

- **Polled** means `src != nil && (src.Polled() || src.Type == "schedule")`,
  the same as the Sources page since #114.
- **A failing source that isn't polled** is left out of `failing`, so no
  new "Source failing" is sent, and its sent episode closes.
- **The recovery body:**
  - "Source X was removed.", when it's missing;
  - "Source X no longer polls.", when it's not polled;
  - else "Source X is polling again."

  The title and the event are unchanged.

## Steps

1. **`internal/notify/notify.go`:** add `polled(cfg *config.Config, name string) bool`.
   Filter the `FailingSources` loop with it, and choose the recovery body.
   - **Tests,** in `internal/notify/notify_test.go` next to
     `TestSourceThresholdAndRecovery`:
     - stopped polling → "no longer polls";
     - removed → "was removed";
     - still polled and failing → no `source_ok`.
   - The clock is injected, as that test already does.

   → verify: `go test -race ./internal/notify`.

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** 1 step and 2 files, so it's implemented by the session. A
fresh Opus agent reviews the diff.

## Rollback

Revert the PR. No state changes.
