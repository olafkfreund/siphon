---
status: draft
issue: 118
intent: intent/2026-10-14-118-portal-refresh.md
---

# Spec: portal refresh, slice 4: Notifications, Egress, Audit, History, Help

This is the last slice (intent decision 1), built on the shared
primitives. The input is a Gemini 4 Argon review at
`scratchpad/agy-slice4.md`. Its bug claims are checked in the code below.
The `.g3` override it also found is fixed in slice 3.

## Verified bugs (fixed here)

1. **Notifications: the add form is shown to non-admins.** The "+
   Channel" button and the `#add` form are gated only by `.Editable`
   (`notify.html:19`), but the submit button needs admin (`:35`).
   Operators and viewers fill in a form they can't submit.
2. **History: Restore has no confirmation** (`config.html:114`). One click
   replaces the live config.
3. **Egress: posture is unclear.** An agent with egress off, meaning full
   network access, reads as muted "egress off", indistinguishable from "3
   hosts" (`egress.html:5`).
4. **Audit: the actor is hidden on phones** (`audit.html:2-3`, `hide-sm`),
   on a page whose subtitle is "who did what".
5. **The cancelled pill has no icon** (`layout.html:45`), unlike every
   other state pill.
6. **Mobile empty states:** `colspan` cells in `table.cards` render at
   half width on phones (in `audit` and `history`).

## Design

- **Notifications:**
  - channels become `.item-card`s: `.is-err` when the last delivery
    failed, the event kinds as `.mode` pills, an exact `title` on times,
    and delivery errors in `.stamp.err` (not muted);
  - "No messages yet" is a plain `.stamp`;
  - the empty test output div is hidden with `:empty`;
  - **bug 1:** the add button and form need `.Editable` **and** admin;
  - recent deliveries become `.item-card.row`s with a filter bar;
  - the "Send me" checkboxes are wrapping chips, not a `.seg` radio
    control.
- **Egress:**
  - each owner is an `.item-card` with a linked name: an agent goes to
    `/config/agents/<n>`, a rule to `/config/rules/<n>`;
  - **bug 3:** a posture stamp with icon and text: "Unrestricted" is
    `.stamp.wait` with `i-shield`, "No network" for an empty allowlist,
    "N hosts" otherwise;
  - refused hosts are `.item-card.row`s marked `.is-err`, with a relative
    time and an exact `title`;
  - the empty states point to the next action.
- **Audit:**
  - `.item-card.row` rows with a filter bar (scope `audit`), searching the
    actor, event, job and detail;
  - **bug 4:** the actor is always shown;
  - job links read `#ID`;
  - **bug 6:** a full-width empty state.
- **History:**
  - the header puts Export on the right (the missing `.sp`);
  - revisions become `.item-card.row`s, with a filter bar once there are
    more than 10;
  - the revision page's back link moves into its header;
  - **bug 2:** Restore gets `data-confirm="Restore revision N? It
    replaces the live config (a new revision you can undo)."`.
- **Help:** a light touch only. The template gallery gets a filter bar
  (scope `templates`, by category and text) if it has more than 12
  entries. Cards use `.item-card`. No content change.
- **Shared:** the `cancelled` pill gets the `i-x` icon (bug 5).
- **No new classes** beyond what slices 1–3 add, except `.chips-check`,
  the wrapping checkbox chips.

## Alternatives rejected

- **Rewriting the help pages:** they're content. Only the layout
  primitives change.

## Risks

- **Tests that grep `/notifications`, `/egress`, `/audit` and `/history`**
  must keep their links and forms.
- **Restore's confirm** adds one click, on a destructive action.

## Verification

- **`internal/web` tests,** one per bug:
  - a viewer gets no `#add`;
  - Restore has `data-confirm`;
  - an unrestricted agent shows "Unrestricted";
  - the audit actor is present without `hide-sm`;
  - the cancelled pill has an icon.
- **Plus:** the filter bars are present and hidden; CSP-clean.
- **`go vet ./... && go test -race ./...`.**
- **Visual on siphon-dev.**
