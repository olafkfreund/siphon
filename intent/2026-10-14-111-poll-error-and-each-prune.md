---
status: draft
issue: 111
author: olafkfreund
---

# Intent: a failed MCP poll says why, and `on: each` state doesn't grow forever

## Problem

Two gaps in existing code. Both matter more now that ServiceNow polling
(#110) is a documented path.

1. **A failed poll tool loses its reason.** When a polled MCP tool returns
   an error (`isError: true`), `internal/source/mcp.go:81-82` reports only
   `MCP tool "search_records" returned an error`. The tool's own message is
   dropped, and with it the reason the poll is broken. For ServiceNow, that
   could be:
   - a 401 or 403;
   - an ACL denial;
   - a hibernating instance;
   - a bad encoded query.

   The source status on the Sources page and in `siphon why` is the only
   place a user sees this.
2. **`on: each` rule state is never pruned.** Every fired id writes a row
   to `rule_state` (`internal/store/store.go:116-124`), and nothing deletes
   it.
   - **Cost:** `RuleLastFiredAll` (`internal/store/query.go:112`) groups
     over the whole table, and `siphon why`'s per-rule listing
     (`internal/store/diag.go:78`) reads every key.
   - **Who hits it:** a ServiceNow poll on a busy instance adds a row per
     incident change. Webhook rules keyed on a delivery id
     (`id: event.id`) add one per delivery.
   - **Result:** these rows grow without limit, while the related
     `seen_event` table is cleaned after 7 days (`store.Cleanup`).

## Proposed outcome

1. **A failed poll shows the tool's message,** for example
   `MCP tool "search_records" returned an error: servicenow: 403 ACL denied`.
   - It's clipped to a sensible length.
   - The secret masking `tick` already applies still covers it.
2. **The daily retention run deletes `on: each` rule state older than the
   seen-events retention** (`server.retention.seen_events`, default 7 days).
   - **What it means:** an id that fired longer ago than that can fire again
     if it ever comes back. That's the same promise the webhook replay check
     makes.
   - **What it leaves alone:**
     - edge rules' state, where a deleted row would make a still-true
       condition refire;
     - any `on: each` id that fired within the retention.

## Affected users and systems

- Anyone with polled MCP sources, or `on: each` rules.
- `internal/source/mcp.go`, `internal/store` (`Cleanup`), and the retention
  call in `internal/job/queue.go:159-166`, which must say which rules are
  `on: each`.

## Constraints

- **No schema migration** if avoidable. `rule_state` can't tell an each id
  from an edge key, so the rule's `on:` comes from the live config at
  cleanup time.
- **Never prune edge state.**
- **No secret in an error.** The tool text goes through the existing
  masking, and it's clipped.
- **Inject the clock in tests,** as the retention tests already do.

## Open questions

1. **The retention to reuse.** I'd reuse `seen_events` (default 7 days),
   since it already means "how long an id counts as seen". The alternative
   is a new `server.retention.rule_ids` knob. **I'd reuse it,** and document
   that it now also covers `on: each` ids.
2. **State left by deleted or renamed rules.** Its rows also stay forever.
   Prune them too, or leave them? **I'd leave them for now.** A rule removed
   and re-added would lose its edge state and could refire, and that's a
   separate decision.
