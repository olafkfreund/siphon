---
status: approved
issue: 93
author: olafkfreund
---

# Intent: the Sources page shows what each source has actually done

## Problem

The Sources page reads only a source's last *poll* time
(`store.SourceStates` → `source_state.last_poll_at`).
- **Webhook sources** record deliveries in `source_state.event_at`, a
  column the page never reads. A webhook that receives events every
  minute still says "Waiting for the first delivery.", with an idle dot.
  Seen on siphon-dev: `test` received an event that ran job 11.
- **MCP sources used only for agent tools** (no `read:`, so never polled)
  say "Not polled yet." for ever. Examples are `servicenow` and
  `github`. They look unconfigured, though their connection test passes.

The user reads "nothing has happened" or "this is broken" when neither is
true.

## Proposed outcome

- **A webhook card** shows "Last delivery <ago>" from `event_at`, and its
  health is ok once a delivery has arrived. A recent refused delivery
  (`last_reject`), if any, is shown as it is on the Connections page.
- **A tool-only MCP card** shows "Agent tools: not polled", plus the last
  connection test's result and time when there is one (it's stored by
  `SetConnectionCheck`). Its health follows that test, and it's never
  "idle" just because it isn't polled.
- **Polled sources** are unchanged.
- **The JSON API** (`/api/diag`, `siphon get sources`) reports the same
  fields, so the CLI and the portal agree.

## Affected users and systems

- Everyone using the portal's Sources page, and the diag API and CLI.
- `internal/store` (the query), `internal/web` (`data.go`, `diagapi.go`,
  `templates/sources.html`).

## Constraints

- **No schema migration.** The columns already exist.
- **No secrets or request content** on the card. Only times, and the
  fixed reject reasons that are already stored.

## Open questions

- None. It's a bug fix. The wording above is a proposal, and you can change
  it at review.
