---
status: draft
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

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** the `coder` agent does steps 1–3. A fresh Opus agent
reviews.

## Rollback

Revert the PR.
