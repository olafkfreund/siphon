---
status: approved
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
