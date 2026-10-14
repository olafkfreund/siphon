---
status: approved
issue: 121
intent: intent/2026-10-14-121-notify-unpolled-recovery.md
---

# Spec: a source that stops polling ends its "Source failing" episode

## Design

In `scanChannel`'s `want("source")` block (`internal/notify/notify.go:167-190`):

- **Collecting failing sources:** in the loop over `store.FailingSources`,
  skip `f` unless `polled(cfg, f.Name)`, where
  `polled = src != nil && (src.Polled() || src.Type == "schedule")`.
  So its key isn't added to `failing`, and no new "Source failing" is
  queued.
- **Recovery:** in the loop over `SentSourceEpisodes`, keep the existing
  `!failing[e.Key]` path. Choose the body by the source's current
  config:

  | Source | Body |
  |---|---|
  | missing | "Source X was removed." |
  | configured but not polled | "Source X no longer polls." |
  | otherwise | "Source X is polling again.", unchanged |

  The title and the event stay "Source recovered" / `source_ok`, so the
  channels and the delivery logic are unchanged.
- **A small helper** `polled(cfg, name) bool` is added next to
  `scanChannel`.

## Alternatives rejected

- **Clearing `failing_since` when config is applied:** it writes state
  from the config path, which is a bigger change. Filtering at read time
  is enough.
- **A new event type,** such as `source_gone`: channels filter by event,
  so existing subscribers would miss it.

## Risks

- **A source that was removed and re-added under the same name** while
  still failing sends "removed", then later a new "Source failing" with a
  new `failing_since` key. That's correct.

## Verification

- **`internal/notify` tests,** following `TestSourceThresholdAndRecovery`
  (`notify_test.go:200`). A source fails past the threshold, and "Source
  failing" is sent.
  - The source's config loses `read`: the next scan queues `source_ok`
    with "no longer polls".
  - Another case removes the source: "was removed".
  - A source that's still polled and still failing gets no recovery.
- **`go vet ./... && go test -race ./...`.**
