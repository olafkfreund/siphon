---
status: approved
issue: 111
spec: spec/2026-10-14-111-poll-error-and-each-prune.md
---

# Plan: a failed MCP poll says why, and `on: each` state doesn't grow forever

## Approved decisions, carried over

- **The poll error:** `MCP tool <name> returned an error: <text>`.
  - The tool name is unquoted, because `stripQuoted` would blank it.
  - `<text>` comes from `toolErrText(result.Content)`: the `TextContent`
    texts joined with `"; "`, whitespace collapsed, and clipped to 300
    bytes on a rune boundary with `…` added. With no text it's
    `(no message)`.
  - `tick` still applies `stripQuoted` and then `action.Mask`, unchanged.
- **Pruning:**
  - `store.Retention` gains `EachRules []string`. The zero value prunes
    nothing.
  - `Cleanup` deletes `rule_state` rows of those rules where
    `last_fired_at < now - r.Seen`, and the row isn't the newest of its
    rule. That keeps `RuleLastFired` and therefore `cooldown`.
  - Edge rules are never listed, so they're never pruned.
  - Rows with a NULL `last_fired_at` are never pruned.
- **The retention** is `server.retention.seen_events`, default 7 days
  (intent decision 1).
- **Deleted rules' state** is out of scope (decision 2).

## Steps

1. **The error text, `internal/source/mcp.go:81-82`.**
   - Change the `IsError` branch as decided, and add `toolErrText` next to
     `jsonText` (`:236` area).
   - **The tests, in `internal/source/mcp_test.go`,** use an in-memory or
     httptest MCP server, following `TestMCPResource` (`:18`), with tools
     that return `IsError: true`:
     - text `boom: 403 ACL` gives an error containing
       `MCP tool fail returned an error: boom: 403 ACL`;
     - 1000 bytes of text gives an error of at most about 300 bytes of
       text plus `…`;
     - no content gives `(no message)`.

   → verify: `go test -race ./internal/source`.

   Traps: clip on a rune boundary (`utf8.ValidString` on the result).

2. **Pruning, `internal/store/store.go`.**
   - **`Retention` (`:288`):** add `EachRules []string`, and update the doc
     comment.
   - **`Cleanup` (`:301-340`):** when `len(r.EachRules) > 0`, run in the
     same transaction:

     ```sql
     DELETE FROM rule_state WHERE rule IN (?,…) AND last_fired_at < ?
       AND last_fired_at < (SELECT MAX(r2.last_fired_at) FROM rule_state r2 WHERE r2.rule = rule_state.rule)
     ```

     The args are the rule names, then `ms(now.Add(-r.Seen))`, using the
     defaulted `r.Seen`. Build the placeholders with `strings.Repeat`,
     never by joining names into the SQL.
   - **The tests, in `internal/store/store_test.go`,** use an injected
     `now`, like `TestCleanupCustomRetention` (`:637`):
     - each rule `e` with keys fired 10, 9 and 1 days ago leaves only the
       1-day key;
     - each rule `old` with one key fired 30 days ago keeps its row;
     - edge rule `g` with rows fired 30 days ago keeps every row;
     - an unfired row (NULL) stays;
     - with `EachRules` nil, nothing is deleted;
     - `RuleLastFired(e)` is unchanged after the prune.

   → verify: `go test -race ./internal/store`.

   Traps:
   - Rule names come from config, but bind them as parameters anyway.
   - Times use the `ms()` helper.

3. **The caller, `internal/job/queue.go:159-166`.**
   - In `retention`, build `each` from `p.Config().Rules`, taking `r.Name`
     where `r.On == "each"`, and pass `EachRules: each`.
   - **The test, in `internal/job`:** following the existing retention or
     cleanup tests (find one with `grep -n Cleanup internal/job/*_test.go`),
     or a direct unit test of a small helper `eachRules(cfg) []string`. It
     checks that each rules are listed and edge rules are not. The helper
     is fine if `retention`'s loop is hard to drive in a test.
   - **The poll-error test:** a polled mcp source whose tool fails gives a
     `store.SourceStates` error containing the tool text. A secret value in
     the text is masked, and a quoted part is blanked. Reuse the
     `internal/job` fake MCP or stdio stub pattern (`aws_test.go:118` reads
     `SourceStates`).

   → verify: `go test -race ./internal/job`.

   Traps: inject the clock, never real-time sleeps.

4. **Docs.**
   - **`docs/tasks/backup-and-monitoring.md:215`:** the `seen_events`
     comment covers `on: each` ids too. Add one sentence after the block:
     an `on: each` id that fired longer ago than this can fire again if it
     comes back.
   - **`internal/config/config.go:121`:** the comment says the same.
   - Run `go generate ./docs`, if `llms-full.txt` includes that page.

   → verify: `go test -race ./docs/...`.

## Deviations

- **The Opus review of the whole diff:** no HIGH, and two MEDIUM.
  - **M2, fixed: the journal got the tool's text unmasked.** `tickAndNudge`
    logged the raw poll error. It now logs `pollErrText`, the same
    blanked, masked, one-line text the source status stores.
  - **L1, fixed: clipping and whitespace-collapse ran before `Mask`,** so a
    clipped or multi-line secret could slip past it, and a clipped closing
    quote could slip past `stripQuoted`. Now:
    - `toolErrText` only joins, capped at 4 KiB;
    - `pollErrText` blanks, then masks, then collapses and clips to 300
      bytes.

    This applies to every stored poll error, not only MCP ones, and
    deviates from step 1's "clip in `toolErrText`".
  - **L2, L3 and L4, fixed:**
    - the rune-boundary test now exercises the back-off;
    - the `seen_events` YAML comment is updated, as spec §3 says;
    - the store test checks its setup errors.
  - **M1, open: an owner decision,** because it changes the spec's premise.
    See the PR or the session.

## Amendment: steps 5–6 (prune by last seen), pending approval

5. **The migration and the sighting** (spec amendment).
   - **The new `internal/store/migrations/0007_rule_state_seen.sql`:**
     `ALTER TABLE rule_state ADD COLUMN last_seen_at INTEGER;`.
   - **`internal/store/store.go`:** add `TouchRuleSeen(tx, rule, key, now)`
     with the throttled UPDATE (`now - 1h`).
   - **`internal/rule/rule.go` `evalItem`, the each branch:** before
     `return f, false, nil` for an already-fired key, call `TouchRuleSeen`
     when `!st.LastFired.IsZero()`, whatever `cur` is.
   - **`eachPrune`:** use `COALESCE(last_seen_at, last_fired_at) < ?`. The
     newest-fired-row guard stays.
   - **The tests:**
     - **Rule:** a touch on re-evaluation, throttled within an hour.
     - **Store:** fired 30 days ago but seen 1 day ago survives; seen 10
       days ago goes; the newest-fired row still stays.
     - **Migration:** the existing migration tests pick up `0007`, so check
       `NewestMigration` and any schema snapshot test.

   → verify: `go test -race ./internal/rule ./internal/store ./internal/job`.

   Traps:
   - **Migrations are append-only:** never edit `0001`–`0006`.
   - **Dry-run evaluation** runs inside a savepoint that rolls back, so
     check that a dry run doesn't write.
6. **The docs.**
   - `docs/tasks/backup-and-monitoring.md` and `internal/config/config.go:121`
     say "not seen (fired, or returned again by a polled source) for longer
     than `seen_events`".
   - Run `go generate ./docs`.

   → verify: `go test -race ./docs/...`.

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** steps 1–4 edit 7 files, so the `coder` agent does them one
at a time. A fresh Opus agent reviews `git diff` against this plan.

## Rollback

Revert the PR. The pruning only deletes rows that today are never read
again, except by `siphon why`'s per-rule key list, which then shows fewer
old keys. No schema change.
