---
status: approved
issue: 118
spec: spec/2026-10-14-118-portal-refresh-slice4.md
---

# Plan: portal refresh, slice 4: Notifications, Egress, Audit, History, Help

## Approved decisions, carried over

- **Six verified bugs:**
  1. the Notifications add form needs `.Editable` plus admin;
  2. History Restore gets `data-confirm`;
  3. an Egress posture stamp: "Unrestricted" `.stamp.wait` with
     `i-shield`, "No network", "N hosts";
  4. the Audit actor is always shown;
  5. the cancelled pill gets `i-x`;
  6. full-width empty states, via `.item-card.row` lists instead of
     `table.cards` with `colspan`.
- **Notifications:**
  - channels as `.item-card` (`.is-err` on a failed last delivery), the
    events as `.mode` pills, errors in `.stamp.err`, exact `title` times;
  - "No messages yet" as a plain `.stamp`;
  - the empty test div is hidden;
  - deliveries as `.item-card.row` with a filter bar;
  - "Send me" as `.chips-check`.
- **Egress:** owner `.item-card`s with linked names; refused rows
  `.is-err`, with relative and exact times.
- **Audit:** `.item-card.row` with a filter bar (scope `audit`); `#ID`
  job links.
- **History:**
  - Export on the right;
  - rows as `.item-card.row` (a filter bar when there are more than 10);
  - the back link moves into the header;
  - Restore gets its confirm.
- **Help:** a template gallery filter bar if there are more than 12;
  `.item-card` cards; no content change.
- **No behaviour change:** the same routes, forms and gates, except the
  admin gate on the add form (bug 1), which matches its submit button.
- **It depends on slice 3** (the `.g3` fix). Start from main after slice
  3 merges.

## Steps

1. **`notify.html`, plus CSS (`.chips-check`).**
   - **The tests:** bug 1, the failed-channel `.is-err`, and the
     deliveries filter bar.

   → verify: `go test -race ./internal/web`.

2. **`egress.html`.**
   - **The tests:** bug 3, the linked owner names.

   → verify: `go test -race ./internal/web`.

3. **`audit.html`, and History in `config.html`.**
   - **The tests:** bugs 2, 4 and 6, and the filter bars.

   → verify: `go test -race ./internal/web`.

4. **Help templates, and the `layout.html` cancelled pill.**
   - **The tests:** bug 5, the gallery filter.

   → verify: `go test -race ./internal/web`.

5. **The visual check (mine).**

## Deviations

- **Steps 1–4:**
  - **The Notifications admin gate** covers the add button, the `#add`
    form and the "Add one below" hint. The submit button's own admin check
    is dropped, because the whole form is now admin-only.
  - **The Notifications Test** uses a relative `div.conn-test` target, not
    `#ntest-<name>`. That's the same dotted-name bug that slice 3 fixed on
    Connections. There's a test with a channel named `a.b`.
  - **The Audit filter bar** has search only: rows have no kind or state
    to filter on.
  - **The gallery filter** chips filter by category. The category headings
    stay visible.
  - **The session deleted** the now-dead `.eg-refused` CSS.

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** the `coder` agent does steps 1–4. A fresh Opus agent
reviews.

## Rollback

Revert the PR.
