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

## Deviations

- **The Opus review:** no HIGH and no MEDIUM.
  - **LOW, accepted:** a source removed or switched off while failing,
    then restored while still failing, keeps its old `failing_since`
    key. Its episode already has a `source_ok` row, so no new "Source
    failing" is sent until it recovers once.
  - It's rare: it needs a config change in both directions while the
    source keeps failing.
  - Clearing `failing_since` for sources that don't poll would be a store
    change, beyond this plan.
- **The test env** now configures `feed` and `blip` as polled http
  sources. Without them the new filter would make the existing recovery
  test pass for the wrong reason.

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** 1 step and 2 files, so it's implemented by the session. A
fresh Opus agent reviews the diff.

## Rollback

Revert the PR. No state changes.
