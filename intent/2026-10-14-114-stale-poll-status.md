---
status: draft
issue: 114
author: olafkfreund
---

# Intent: a source's status shows only real poll failures, and only while it polls

## Problem

On siphon-dev, the `servicenow` mcp source showed a red **context
canceled** card on the Sources page, although nothing was wrong with it.

1. **A cancelled poll was stored as a failure.** A config change removed
   the source's `read` and `poll`, and the reload cancelled the poll that
   was in flight. `tick` (`internal/job/pipeline.go:201-209`) stores every
   poll error with `PutSourceState`, the cancellation included. A shutdown
   during a poll does the same.
2. **Nothing replaced it.** Once the source stopped polling, nothing ever
   overwrote that error. The Sources page (`internal/web/data.go:102-104`)
   shows `LastPollAt`/`LastError` for every source, whether it polls or
   not. The same stale state reaches `siphon why`
   (`internal/web/diagapi.go:233,282`), as "Source X is failing".

## Proposed outcome

- **A poll cut short by a reload or a shutdown** isn't recorded as the
  source's error. The previous status stands.
- **The poll status shows only for sources that poll** (`src.Polled()`):
  the last poll time, the last error and failing-since. A source that
  only serves agent tools, a webhook or a schedule never shows a poll
  error.

## Affected users and systems

- Anyone who changes or reloads config while sources poll, and anyone who
  turns polling off on a source.
- `internal/job/pipeline.go` (`tick`), `internal/web/data.go` (`sources`),
  and `internal/web/diagapi.go` (`siphon why`).

## Constraints

- **A real poll failure still shows,** including a timeout, which is not
  a cancellation from a reload.
- **No migration.** Stale rows already in the database are simply not
  shown, because the source no longer polls.
- **Inject the clock in tests,** with no real sleeps.

## Open questions

None. Both fixes are in the issue; the only judgement is telling the two
kinds of cancellation apart. A context cancelled by Siphon (a reload or a
shutdown) is ignored. A poll's own timeout is a real failure. The spec
will say how they're told apart.
