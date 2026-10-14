---
status: approved
issue: 118
spec: spec/2026-10-14-118-portal-refresh-slice2.md
---

# Plan: portal refresh, slice 2: Approvals and Sources

## Approved decisions, carried over

- **It builds on slice 1's primitives.** Start from main after slice 1
  merges.
- **Approvals:**
  - `.item-card.is-wait`, with `data-kind`;
  - the header: the rule, `#ID`, and a wait/expiry `.stamp` (`.err` when
    under 15 minutes, `Soon` set in Go);
  - the body: a `.flow.flow-2` from the trigger (source) to the `run`,
    with the provider;
  - the footer: the expiry, Details, Deny (`data-confirm`, `aria-label`)
    and Approve (`aria-label`);
  - viewers see a note;
  - the filter bar (scope `approvals`) when there are two or more;
  - the forms and gates are unchanged.
- **Sources:**
  - `.item-card` with the source-kind hairline, `data-health`, `data-hot`
    and its state class;
  - a text health stamp, never colour alone;
  - an endpoint node: the webhook URL with Copy, the schedule's `at`, or
    the URL or stdio;
  - `.blocked` callouts for the last error and the last refusal;
  - footer pills, and the OAuth and Notion controls, with the status text
    shown to all roles;
  - Edit and New links for admins;
  - the filter bar (scope `sources`).
- **The view data:**
  - `sourceView.Target`, `.Rules`;
  - `Hot` also counts `LastRunAt`;
  - "stale" when a webhook is refusing or the OAuth login is expired;
  - the approvals' action summary gains `Source`, `SourceType` and `Soon`.
- **`srcicon`:** schedule uses `i-timer`.
- **New CSS:** `.flow.flow-2`, the source-kind hairlines, `.blocked.is-wait`
  and `.mode .ico`.

## Steps

1. **The view data** (`data.go`, the approvals handler): the fields above.
   - **The tests:** hot via `LastRunAt`, the stale cases, the webhook
     `Target` with and without `public_url`, the rule count, and the
     approvals' `Source` and `Soon`.

   → verify: `go test -race ./internal/web`.

2. **`approvals.html`**, plus the CSS for `.flow-2`.
   - **The tests:** per the spec, approvals.

   → verify: `go test -race ./internal/web`.

3. **`sources.html`**, plus `srcicon` and the source-kind CSS.
   - **The tests:** per the spec, sources.

   → verify: `go test -race ./internal/web`.

4. **The visual check (mine).**

## Deviations

- **Steps 1–3:**
  - **Notion:** viewers see the status text. The token value and the "Use
    this token" form stay operator-only, as `TestNotionVerification`
    requires.
  - **A stdio source's `Target`** shows only the package or `argv[0]`,
    never the arguments, which may carry values.
  - **The expired-OAuth "stale" case** is implemented but untested: a
    token store in the web tests is too heavy. The refusing-webhook case
    is tested.
  - **The approvals stamp** reads "asked X ago · expires in Y", with a new
    `left` template func. The session changed "waiting" to "asked".
  - **`ActionOf`** is `map[int64]approvalAction`, which wraps
    `store.JobAction` with `Source`, `SourceType` and `Soon`. A pending
    job outside the dashboard's 24-hour window shows "unknown" as its
    trigger, with "see details".
- **The Opus review:** no HIGH. One MEDIUM, fixed.
  - **MEDIUM:** http and mcp source URLs were shown in full, so a key in a
    query string (`?api_key=…`) reached every viewer. `displayURL` keeps
    the scheme, host and path only, with a test.
  - **LOW, fixed:**
    - **The approvals page** loads every waiting job through `runs()`,
      not the dashboard's newest 20. (The "outside the 24 h window"
      deviation above had the cause wrong.)
    - **The Dashboard** counts `stale` sources ("N need a look"), so it no
      longer says "all healthy" next to a refusing webhook.
    - **An expired approval** reads "expired", not "expires in expired".
    - **`approvalSoon`** is its own constant.
    - **A hot source's stamp** says "active", not colour alone.
    - **The provider assertion** matches `>Claude<`.
  - **LOW, accepted:** the approval card's trigger comes from the live
    rule (the spec's choice).

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** the `coder` agent does steps 1–3. A fresh Opus agent
reviews.

## Rollback

Revert the PR.
