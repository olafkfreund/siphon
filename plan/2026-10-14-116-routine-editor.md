---
status: approved
issue: 116
spec: spec/2026-10-14-116-routine-editor.md
---

# Plan: creating a routine starts from recipes and help, not a blank box

## Approved decisions, carried over

- **When `not .HasForm`,** the YAML form is a non-collapsible
  `section.card` with an `<h2>`.
- **A new, empty routine** gets the commented starter: `fetch` (cmd) then
  `summarise` (agent), using `{{.steps.fetch.output}}`. A test parses it
  as a `config.Routine`.
- **Recipes:**
  - `docs.Templates()` with `routines:`;
  - `GET /config/routines/new?recipe=<name>` prefills `routines[0]`'s
    name and body, server-side;
  - a note lists the template's other items, with a link to
    `/help/templates#<name>`;
  - an unknown name gives no prefill.
- **The reference panel** (routines only, `.grid.g2`):
  - the `config.Explain("routine")` fields;
  - `button.token[data-insert]` chips for each step ID, plus
    `{{.event.FIELD}}`;
  - in JS, inserting at the caret fires `input`, which runs #117's
    validation.
- **Built on #117 and #118 slice 1:** rebase after both merge.
- **No guided step builder.**

## Steps

1. **`internal/web/editors.go`:** the starter `const` and its use when new
   and empty. Add a `Recipes`, `Recipe` and `RefFields` view data, and the
   tokens from `v.Steps`.
   - **The tests:**
     - the starter parses as a `config.Routine`;
     - `editExtras` gives the tokens for the steps.

   → verify: `go test -race ./internal/web`.

2. **`internal/web/configweb.go`:** the `?recipe=` prefill for
   `kind == "routines" && isNew`, which finds the template, parses it,
   takes `routines[0]` and lists the other items.
   - **The tests:**
     - `?recipe=weekly-digest` prefills;
     - an unknown recipe doesn't.

   → verify: `go test -race ./internal/web`.

3. **`templates/config.html`:**
   - the non-collapsible section when `not .HasForm`;
   - the recipe card, when new;
   - the `.grid.g2` layout with `aside.card.ref` for routines.
   - **The tests:**
     - no `<details` around the routine form;
     - the recipe links;
     - the reference tokens;
     - the CSP test.

   → verify: `go test -race ./internal/web`.

4. **`static/app.js` + `static/style.css`:**
   - `[data-insert]`, inserting at the caret;
   - `.ref`, `.token` and `.recipes` styles, from #118's primitives.

   → verify: the visual check.

5. **The visual check (mine), on siphon-dev.**

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** steps 1–4 edit files, so the `coder` agent does them. A
fresh Opus agent reviews.

## Rollback

Revert the PR. Additive, plus the collapse fix.
