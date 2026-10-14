---
status: approved
issue: 121
author: olafkfreund
---

# Intent: a source that stops polling ends its "Source failing" episode

## Problem

The notifier, `internal/notify/notify.go` `scanChannel` (`:167-190`),
treats every `source_state` row with `failing_since` as still failing. A
source that failed long enough to send **Source failing**, and then
stopped polling, never gets **Source recovered**. That happens when its
`read:`/`poll:` is removed, or the source is deleted. Its `failing_since`
is never cleared, because nothing polls it any more. Since #114, the
Sources page already shows it without the error, so notifications and the
page disagree.

## Proposed outcome

- **The episode ends.** A failing source that's no longer polled, or no
  longer exists, counts as not failing.
- **What's sent:** if a "Source failing" message went out for it, the
  channel gets a closing message: "Source X no longer polls" when it's
  still configured, "Source X was removed" when it's gone. It doesn't say
  "is polling again", which would be false.
- **No new "Source failing"** message is sent for such a source.

## Affected users and systems

- Anyone with notification channels subscribed to `source` events.
- `internal/notify/notify.go` and its tests.

## Constraints

- **The recovery path stays as it is:** the same `source_ok` event and the
  same key. Only the body changes for these sources, and the delivery
  logic is untouched.
- **"Polled"** means what the Sources page uses since #114:
  `src.Polled() || src.Type == "schedule"`.

## Open questions

None. The owner pre-approved the recommendations.
