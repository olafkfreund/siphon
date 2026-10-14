---
status: draft
issue: 111
intent: intent/2026-10-14-111-poll-error-and-each-prune.md
---

# Spec: a failed MCP poll says why, and `on: each` state doesn't grow forever

## Design

### 1. The tool's error text, `internal/source/mcp.go:81-82`

Today:

```go
if result.IsError {
    return Event{}, fmt.Errorf("MCP tool %q returned an error", o.Tool)
}
```

Change it to:

```go
if result.IsError {
    return Event{}, fmt.Errorf("MCP tool %s returned an error: %s", o.Tool, toolErrText(result.Content))
}
```

- **`toolErrText`** joins the `TextContent` texts with `"; "`, collapses
  whitespace, and clips to 300 bytes on a rune boundary, with `…` added.
  With no text it gives `(no message)`.
- **The tool name is unquoted.** That's deliberate: `tick` runs every poll
  error through `stripQuoted`, which blanks quoted literals
  (`internal/job/pipeline.go:848-852`). That blanking keeps event values
  out of the stored status, but it also turns today's `%q` tool name into
  `"…"`. A tool name is config, not data.
- **The tool text then goes through the same two filters** in `tick`
  (`pipeline.go:204-205`):
  - `stripQuoted`, so quoted values in the message are blanked, the same
    policy as rule errors;
  - `action.Mask` with `cfg.Secrets()`.

  Nothing new is stored unfiltered.
- **ServiceNow's message** is `error.message`, or the HTTP status text
  (`internal/servicenow/client.go:324-334`), so it carries no token.

### 2. Pruning `on: each` rule state, `internal/store/store.go` `Cleanup` (`:301`)

- **The signature:** `Retention` gains `EachRules []string`, the names of
  the live config's `on: each` rules. Its zero value prunes nothing, so
  every existing caller and test keeps today's behaviour.
- **One more statement in `Cleanup`'s transaction,** when `EachRules` isn't
  empty:

  ```sql
  DELETE FROM rule_state
   WHERE rule IN (…EachRules…)
     AND last_fired_at < :cut
     AND last_fired_at < (SELECT MAX(last_fired_at) FROM rule_state r2 WHERE r2.rule = rule_state.rule)
  ```

  Here `cut = now - r.Seen`, the same cut as `seen_event`.
- **Why the newest row is kept:** `RuleLastFired` (`store.go:173-180`) is
  `MAX(last_fired_at)` per rule, and it drives `cooldown`. A rule whose
  last fire is older than the retention, but whose `cooldown` is longer,
  would otherwise lose its cooldown. So the newest row per rule always
  stays.
- **Edge rules are never in `EachRules`,** so their rows are never touched.
- **Unfired rows aren't pruned.** A row with a NULL `last_fired_at` never
  matches `<`.
- **The caller, `internal/job/queue.go:159-166` `retention`:**
  - it builds `EachRules` from `p.Config().Rules`, for rules with
    `On == "each"`;
  - it reads them at each run, the same way it already reads the retention.

### 3. Docs

- **`docs/tasks/backup-and-monitoring.md:215`:** the comment becomes
  `ids used to drop duplicate webhook deliveries, and the ids an on: each rule has fired (default 168h = 7 days)`.
  Add one sentence after the block: an `on: each` id that fired longer ago
  than this can fire again if it comes back.
- **`internal/config/config.go:121`:** the comment says the same.

## Alternatives rejected

- **A schema column marking each rows,** with a migration. The live config
  already knows which rules are `on: each`, so no migration is needed.
- **A separate `rule_ids` retention.** It was rejected at the intent
  (decision 1).
- **Pruning deleted rules' state.** That's out of scope (decision 2).
- **Keeping the quoted `%q` tool name.** `stripQuoted` blanks it anyway.
- **Showing the tool text without `stripQuoted`.** It's the same data class
  as rule errors, so it gets the same policy.

## Risks

- **An id that comes back after the retention fires again.**
  - For webhooks, that matches the replay window: a delivery that old is
    already refused by the signature timestamp.
  - For polling, the query window, 10 minutes by default, stops an old
    record coming back unchanged.
  - A user with a far longer custom poll window, over 7 days, could see an
    old record refire. The docs sentence covers this.
- **Turning a rule from `on: each` to edge** leaves its old rows unpruned,
  which is today's behaviour.
- **Speed:** the correlated `MAX` subquery runs once a day over
  `rule_state`. It uses the primary key `(rule, key)`'s `rule` prefix.
- **A clipped message** can hide the end of a long error. 300 bytes is
  enough for ServiceNow and most MCP servers.

## Verification

- **`internal/source`:** a fake MCP server whose tool returns
  `isError: true` with text `boom: 403 ACL`. The poll error contains
  `search_records returned an error: boom: 403 ACL`. A 1000-byte text is
  clipped with `…`, and empty content gives `(no message)`.
- **`internal/job`:** a polled source whose tool fails stores a source
  status that includes the tool text, masked when it holds a secret value,
  and with any quoted parts blanked.
- **`internal/store` `TestCleanup`-style tests,** with an injected `now`:
  - an each rule with 3 keys, fired 10, 9 and 1 days ago, keeps only the
    1-day key;
  - an each rule whose only key fired 30 days ago keeps that newest row;
  - an edge rule with old rows keeps every row;
  - a zero `EachRules` deletes nothing.
- **`internal/job`:** `retention` passes the config's `on: each` rule names.
- **`go vet ./... && go test -race ./...`.**

## Amendment (after the review of the implementation): prune by last seen, not last fired

**Why it changed.** The review found that the Risks premise above is wrong
for polls that return current state: MCP resources and tools, and HTTP. An
id still in the poll result after `seen_events` (7 days) would lose its row
and fire again. For example, `mcp-task-triage` would rerun its agent every
week for a task that stays failed. The owner chose last-seen tracking.

**The design** replaces the `last_fired_at < cut` condition in §2:

- **The migration `internal/store/migrations/0007_rule_state_seen.sql`:**
  `ALTER TABLE rule_state ADD COLUMN last_seen_at INTEGER;`. Existing rows
  are NULL and count as their `last_fired_at`.
- **Recording a sighting, in `internal/rule/rule.go` `evalItem`, the
  `on: each` branch.**
  - **When:** the key's row has already fired (`!st.LastFired.IsZero()`),
    whatever `when` says now. The id is in this event, so it's still alive.
  - **What:** it calls `store.TouchRuleSeen(tx, rule, key, now)`. The
    function runs
    `UPDATE rule_state SET last_seen_at=? WHERE rule=? AND key=? AND (last_seen_at IS NULL OR last_seen_at < ?)`,
    with `now` and `now - 1h`.
  - **The cost** is at most one write per id per hour.
  - **Dry runs** are covered: they run inside the savepoint that already
    rolls back.
- **The prune:**
  `… AND COALESCE(last_seen_at, last_fired_at) < :cut AND last_fired_at < (SELECT MAX(last_fired_at) …)`.
  The newest-fired row per rule still stays, for cooldown. Edge rules are
  still never listed.
- **Webhook and schedule rules** are never sighted again, so they prune by
  fire time, as before.
- **The docs** say: "an `on: each` id that has not been seen (fired, or
  returned again by a polled source) for longer than `seen_events` can fire
  again if it comes back".

**The risks this adds:**
- **Writes:** one UPDATE per already-fired id per poll, at most once an
  hour. For a 50-record poll every 2 minutes, that's at most 50 small writes
  an hour.
- **The migration** adds a nullable column, so no data is rewritten.
- **An id refires only if it vanished from every poll for longer than
  `seen_events`,** then came back. That's the intended meaning.

**More verification:**
- **The rule test:** a fired each id evaluated again sets `last_seen_at`.
  A second evaluation within an hour doesn't write. One after an hour does.
- **The store test:** a row fired 30 days ago but seen 1 day ago survives
  the prune, and one seen 10 days ago goes. The newest-fired row still
  stays.
- **The migration** applies on a database at `0006`.
