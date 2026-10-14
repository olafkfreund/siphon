---
status: draft
issue: 118
intent: intent/2026-10-14-118-portal-refresh.md
---

# Spec: portal refresh, slice 3: config lists (Agents, Routines), Services and Connections

This is the third slice (intent decision 1), built on slice 1's shared
primitives on main. The input is a Gemini 4 Argon review (intent decision
2) at `scratchpad/agy-slice3.md`. Its bug claims are **verified in the
code** below.

## Verified bugs (fixed in this slice)

1. **Services: a search drops the category.** `services.html:26-31`: the
   `<input type="hidden" name="cat">` sits in the search form, *outside*
   `#service-results`. A category chip swaps only `#service-results`, so
   the form keeps the old `cat`, and the next keystroke searches the wrong
   category.
2. **Connections: a name with a dot breaks Test.** `logins.html:58-60`:
   `id="test-{{.Name}}"` and `hx-target="#test-{{.Name}}"`. A name such as
   `ollama.lan`, which the form's pattern allows, becomes a CSS
   class selector, so htmx can't find the target.
3. **Connections: the login form is a radio trap.** `style.css:350-356`:
   switching the provider from Claude to Codex or agy hides the "token"
   choice, but it stays `:checked`. The instructions go blank, and the form
   submits an invalid `provider`/`kind` pair. agy plus apikey shows the
   API-key help.
4. **A duplicate `.g3`.** `style.css:482` (in the help section) redefines
   `.g3` as `repeat(3, 1fr)` globally, overriding the responsive rule at
   `:332` for every page wider than 900px.

## Design

### Config lists (`cfglist`, every kind)

- **The kind nav** becomes `.chips` with `.chip[aria-current=page]`. Today
  the current kind isn't styled.
- **The table** becomes `.item-card.row` rows in a `[data-filter-list]`.
  - Each row: the name link, the provenance badge (file, portal,
    override, deleted), and Reset (admin, with
    `data-confirm="Reset X to the file version?"`).
  - A `.filter-bar` (search) appears when there are more than 6.
- **Agents** rows add:
  - the `provider` badge, from `agentProvider`;
  - the model;
  - the credential;
  - MCP sources as `.mode` pills;
  - "egress limited" when enforced;
  - "used by N rules".
- **Routines** rows add:
  - the step count and a compact list of the step kinds, as `kindicon`
    chips, in order;
  - "asks approval" when any step has `approve`;
  - "used by N rules".
- **The empty state** links to New `<kind>` (admin) and to templates.
- **The view data:** `cfgRow` gains `Provider`, `Model`, `Credential`,
  `MCP []string`, `Egress bool`, `Steps []string` (the kinds), `Approval
  bool` and `Rules int`. They're filled in `configweb.go` when the kind is
  agents or routines, from the effective config.

### Services (`services.html`)

- **Bug 1:** the hidden `cat` input moves inside `#service-results`, linked
  to the search form with `form="…"`, so a chip swap carries the right
  category.
- **Category links** become `.chips`/`.chip[aria-current=page]`.
- **Availability pills** get an icon plus text: available `i-check`,
  needs-package `i-unit`, not-yet none.
- **Tiles:** the Connect button shows only for admins. Others see "an
  admin connects services".
- **Connected services** become `.item-card` rows: the name, the
  capability `.mode` pills, and a status.
- **The last event** is relative (`agop`, with an exact `title`), and is
  shown only for services with webhooks or polling. A tools-only service
  says "agent tools".
- **The view data:** `connRow` gains `LastEventAt *time.Time`, and keeps
  `LastEvent` for the API.

### Connections (`logins.html`)

- **Bug 2:** the test output is a `div.conn-test` next to its form, and
  the form uses `hx-target="next .conn-test"`, a relative target, with no
  id built from the name.
- **Bug 3:**
  - **CSS:** API-key help is hidden when agy is checked.
  - **`app.js`:** on a provider change, if the checked `kind` radio is
    hidden, it checks `login`. A small addition to the existing `change`
    listener.
- **The cards** become `.item-card`s:
  - `.is-err` when expired, or when the last test failed;
  - `.is-ok` when working;
  - a `.dot` with text status;
  - a footer saying "used by N agents" or "unused".
- **The add forms and buttons** show only for admins.
- **The view data:** `loginView`, and the model view, gain `Agents []string`
  (the agents whose credential is this one).

### Shared

- **Bug 4:** remove the global `.g3` override at `style.css:482`, and
  scope it to `.helplayout .g3` if the help page needs three columns.
- **New CSS:**
  - `.item-card.is-ok::before` (`--kelp`);
  - `.conn-test` (`:empty` hidden);
  - `.steps-mini`, an inline row of `kindicon` chips for a routine's step
    kinds. That's simpler than the review's `.step-chain` list.

## Alternatives rejected

- **The review's `.step-chain` vertical list on routine rows:** too tall
  for a list. The kinds as inline chips are enough, and the editor shows
  the steps.
- **Removing the connected-service edit popover:** out of scope. It's a
  behaviour change, so only its styling aligns.

## Risks

- **`cfglist` serves every kind.** Only agents and routines get extra
  data, and the others render the same rows as now, with the new
  primitives.
- **Tests that grep `/config/<kind>` and `/connections`** must keep their
  links.
- CSP, contrast, and no JS.

## Verification

- **`internal/web` tests:**
  - **Services:** after a category chip, the hidden `cat` is inside
    `#service-results` with the new value.
  - **Connections:** a connection named `a.b` renders a `.conn-test`
    target and no `#test-a.b`.
  - **The login radio,** in JS only: a manual check.
  - **`.g3`:** the global override is gone.
  - **Agents list:** the provider badge, model, "used by N rules".
  - **Routines list:** the step-kind chips and the approval pill.
  - **The Reset button** has `data-confirm`.
  - **The kind nav** has `aria-current`.
  - **The availability pills** have icons.
  - **For a viewer:** no Connect, and no add forms.
  - **CSP-clean.**
- **`go vet ./... && go test -race ./...`.**
- **Visual on siphon-dev.**
