---
status: draft
issue: 116
author: olafkfreund
---

# Intent: creating a routine starts from recipes and help, not a blank box

## Problem

`/config/routines/new` is a bare Name field over an empty YAML textarea:

- **No form or help.** Rules, sources and agents get a guided form and a
  preview (`HasForm`). Routines get only the YAML, with no starter, no
  examples and no reminder of the fields or of how steps pass output to
  each other (`steps.<id>.output`, `.json.<key>`, `.exit`).
- **A real bug.** With no form, the whole editor is a `<details>`
  (`templates/config.html:38`), so clicking its "Edit as YAML" heading
  collapses everything, Save included.
- **Unused examples.** Thirteen ready-made task templates in
  `docs/templates/` already contain good routines, and none of them is
  offered here.

## Proposed outcome

- **The editor can't be collapsed** when YAML is the only way to edit.
- **A new routine starts from a commented starter:** two steps that show
  `cmd`, `agent` and passing output from one step to the next.
- **A recipe picker:** "Start from a recipe" lists the routines from
  `docs/templates`, by title. Picking one fills the editor with that
  routine. It's server-side, via `?recipe=`, so it works without JS. The
  page notes what else the template needs (its rule, agent and sources)
  and links to the full template.
- **A side reference panel:**
  - the routine and step fields, from `siphon explain routine`;
  - the output tokens for the steps in the current YAML (`steps.<id>.output`,
    `steps.<id>.exit`, `{{.steps.<id>.json.KEY}}`);
  - clicking a token inserts it at the caret, a JS enhancement on top of
    #117's editor.

## Affected users and systems

- Admins who create or edit routines in the portal.
- `internal/web/templates/config.html`, `internal/web/editors.go`
  (`editExtras`, the routines case), `internal/web/configweb.go` (the
  `?recipe=` prefill), `static/app.js` and `static/style.css`.

## Constraints

- **The same portal constraints:** CSP, no library, works without JS,
  accessible.
- **No new endpoint for writing.** The recipe fills the form only, and
  Save works as today.
- **Built on #117** (the editor) and #118 slice 1 (the shared styles).
- **A full guided step builder is out of scope,** to be decided after
  this ships.

## Open questions

None. The owner pre-approved the recommendations.
