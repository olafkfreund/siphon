---
status: draft
issue: 116
intent: intent/2026-10-14-116-routine-editor.md
---

# Spec: creating a routine starts from recipes and help, not a blank box

## Design

### 1. The collapse fix, `templates/config.html:38-44`

When `not .HasForm`, render the YAML form inside `<section class="card mb
yaml-edit">` with an `<h2>YAML</h2>`. Otherwise keep the `<details>`, as
it's only optional there.

### 2. The starter, `editors.go` `editExtras` (the routines case)

When `v.New && strings.TrimSpace(v.YAML) == ""`, set `v.YAML` to:

```yaml
# A routine runs its steps in order. Each step is one of cmd, agent, unit or servicenow.
steps:
  - id: fetch
    cmd: [curl, -fsS, "https://example.com/status"]
  - id: summarise
    agent: CHANGE-ME        # an agent from Agents
    # its prompt can use the previous step: {{.steps.fetch.output}}
```

It's a plain `const`, and the field names are checked against
`config.Routine` in a test that parses it.

### 3. Recipes

- **The list:** `docs.Templates()` filtered to those whose YAML has
  `routines:`. For each one, the name and the title are shown, and the
  first routine's name.
- **The prefill:** `GET /config/routines/new?recipe=<template>`. In
  `configweb.go`, the view for new routines finds that template, parses
  it, takes `routines[0]`, and prefills `Name` with the routine's name and
  `YAML` with its body.
- **What the template needs:** a `.Recipe` note lists the template's other
  items (rules, agents, sources, from parsing) with a link to
  `/help/templates#<name>`: "apply the whole template to get these".
- **An unknown recipe** shows no prefill, and a muted "unknown recipe".
- **The markup:** a `.card` "Start from a recipe" above the editor, only
  when `.New`. It's a list of links, so it needs no JS.

### 4. The reference panel

- **The layout:** for routines, the page uses `.grid.g2`: the editor on
  the left, and `aside.card.ref` on the right.
- **Fields:** `config.Explain("routine")` gives the field rows (name and
  short description). The step fields are shown compactly.
- **Outputs:** for each step ID in `v.Steps` (already parsed in
  `editExtras`), three `button.token[data-insert]` chips:
  `{{.steps.ID.output}}`, `{{.steps.ID.json.KEY}}` and `steps.ID.exit == 0`.
  Plus `{{.event.FIELD}}`.
- **The JS (`app.js`):** a click on `[data-insert]` inserts its text at the
  textarea's caret with `setRangeText`, focuses it, and fires an `input`
  event, so #117's validation runs. Without JS, the tokens are plain
  `<code>`, which the user can copy.

## Alternatives rejected

- **A client-side recipe picker:** the server can prefill, which works
  without JS and needs no template data in JS.
- **The full guided step builder:** out of scope (intent).

## Risks

- **A template routine names agents or units that don't exist here.** Save
  validates and says so, and the recipe note explains what to create.
- **Starter drift from the schema:** a test parses the starter as a
  `config.Routine`.

## Verification

- **`internal/web`:**
  - **The collapse fix:** `/config/routines/new` has no `<details`
    around the form.
  - **The starter** is present and parses.
  - **`?recipe=weekly-digest`** prefills that routine's name and steps,
    and shows the needs note.
  - **An unknown recipe** shows no prefill.
  - **The reference panel** lists the fields and, for an existing routine
    with steps `a` and `b`, the `{{.steps.a.output}}` tokens.
  - **CSP-clean.**
- **`go vet ./... && go test -race ./...`.**
- **Visual on siphon-dev:**
  - start from a recipe;
  - insert a token;
  - see validation (#117) react;
  - collapse can't hide Save.
