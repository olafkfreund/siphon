---
status: draft
issue: 115
intent: intent/2026-10-14-115-rules-page-refresh.md
---

# Spec: the Rules page is easy to scan, search and filter, and looks alive

## What exists (checked in the code)

- **The template:** `internal/web/templates/rules.html` is a single
  template. Each rule is a `section.card.rule-card`, with a `.rule-head`
  holding the name, the override badge, Edit, the fired text and the
  switch form. Then come the `.flow` (three `.node`s and two `.pipe`s) and
  an optional cooldown line.
- **The view data:** `internal/web/data.go:76-89` `ruleView`, built in
  `rules()` (`:169-200`). It has Name, Source, SourceType, When, On,
  Cooldown, Kind, Target, Action, Enabled, Overridden and LastFired. It
  doesn't have approval, provider, for_each, repeat or egress.
- **The icons:** `layout.html:47-51` has `kindicon` (agent, cmd, unit,
  routine, else bolt), `srcicon` (webhook, http, mcp, else src) and
  `provider` (monogram badges).
- **The styles,** in `static/style.css`:
  - `.dot.ok`, `.stale`, `.error`, `.idle` (`:111-113`);
  - `.chips` and `.chip` (`:126-128`);
  - `.badge.ovr` (`:159`);
  - `.flow`, `.node`, `.pipe` (`:134-150`). The pipes already animate with
    `flowx`, and `.flow.off` stops them;
  - a global `prefers-reduced-motion: reduce` kill switch (`:352`);
  - `.run-ico.k-agent` and its siblings (`:219`).
- **Approval:** `config.NeedsApproval(r)` (`config.go:619`) is the rule's
  `approve`, or the agent's.
- **The agent provider:** `pages.go:264-266` uses the agent's `Kind`.
- **Search:** `static/app.js:53-62` already focuses the first
  `input[type=search]` on `/`.

## Design

### 1. View data, `internal/web/data.go`

`ruleView` gains these fields, all set in `rules()`:

| Field | Value |
|---|---|
| `Approval bool` | `cfg.NeedsApproval(r)` |
| `Provider string` | for agent actions, `cfg.Agents[name].Kind` (the same source as `pages.go`) |
| `ForEach bool` | `r.ForEach != ""` |
| `Repeat string` | `time.Duration(r.Repeat).String()` when it's above 0 |
| `Egress bool` | `r.Egress` has any allowed host. Check the `EgressRule` shape when implementing. |
| `Hot bool` | `Enabled && LastFired != nil && now.Sub(*LastFired) < 15*time.Minute`, with `now` from the server's injected clock (intent decision 1) |

The fields have JSON tags, so `/api` consumers get them too, and nothing
is removed.

### 2. Markup, `internal/web/templates/rules.html`

- **A toolbar,** above the grid and shown only when there's at least one
  rule:
  - `input[type=search].rules-search` (`aria-label="Filter rules"`,
    placeholder "Filter rules…  /");
  - a `.chips` group of `button.chip[data-filter]`: All, then agent, cmd,
    unit, routine and servicenow (only the kinds present), then enabled,
    disabled and fired recently. Intent decision 2.

  The toolbar is `hidden` in the markup, and `app.js` reveals it, so a
  no-JS page shows only the full list.
- **The card:** `section.card.rule-card` gains `data-kind`,
  `data-status="on|off"`, `data-hot` and `data-text`. The last one is the
  lower-cased name, source, when and target for the search. It also gets
  class `is-hot` or `is-off`.
- **The header:**
  - on the left, `.dot` (`ok` when hot, `idle` when enabled and not hot, a
    new `off` when disabled), the name as a link to `/config/rules/<name>`,
    and the override badge;
  - then `.sp`;
  - on the right, a `.fired` pill, then Edit, then the switch. That order
    stays stable.
- **The flow:**
  - **The source node:** `{{template "srcicon" .SourceType}}`, the type
    label, and the name.
  - **The condition node:** the trigger as a small pill ("each" or
    "edge"), then the expression.
  - **The action node:** `{{template "kindicon" .Kind}}`, the kind label,
    and the target in `.node-target` with a `title` holding the full
    target. For agents, `{{template "provider" .Provider}}`.
  - The repeated words "Source" and "When" go. The pill and the icons
    carry the meaning, and screen readers keep it through a visually
    hidden `.sr` label.
- **The footer,** `.rule-foot`, always present: pills for cooldown,
  approval ("asks approval"), for_each, repeat and egress. With none of
  them, it reads "runs automatically".
- **The empty state:** the existing bubbles card, plus links to
  `/help/templates` ("start from a template") and to New rule. The
  admin-only link keeps its `can` check.
- **The filtered-empty state:** a `p.rules-none[hidden]`, "No rules
  match", with a `button[data-action=clear-filter]`.

### 3. Styles, `internal/web/static/style.css`

Everything here reuses the existing tokens: `--ink`, `--current`,
`--biolume`, `--kelp`, `--amber`, `--coral`, `--line`, `--surface`.

- **`.flow`** becomes a CSS grid:
  `minmax(6.5rem,.85fr) 30px minmax(10rem,1.5fr) 30px minmax(8rem,1.1fr)`.
  The existing `@container` stack for narrow cards stays.
- **`.node-target` and the source name:** `white-space: nowrap`,
  `overflow: hidden`, `text-overflow: ellipsis`. That replaces the
  `overflow-wrap: anywhere` that splits identifiers.
- **The condition's `code`:** `overflow-wrap: break-word` stays (an
  expression may wrap), but `hyphens: none`, and it's clamped to 3 lines.
  The full text is in the `title`.
- **A top accent hairline per action kind:** `.rule-card::before`, 3px,
  coloured by `data-kind` (agent `--ink`, cmd `--current`, routine
  `--biolume`, unit `--amber`, servicenow `--kelp`). Disabled cards use
  `--line`.
- **`.is-hot`:**
  - a `--biolume` border tint, and a soft outer glow (box-shadow);
  - a highlighted `.fired` pill;
  - pipes in `--biolume`.

  Under `prefers-reduced-motion: reduce`, the global rule already stops the
  animation. The glow is static, so it stays.
- **`.is-off`:** a dashed border, the existing `.flow.off` dimming, and a
  new `.dot.off` (hollow, `--line`).
- **New classes:** `.rule-foot` (a border-top hairline, a muted pill row),
  `.rules-bar`, `.rules-search`, `.rules-none`, `.sr` (visually hidden),
  `.trig` (the trigger pill).
- **Hover:** a 1px lift on the card, with `transition` on transform and
  border only. Focus-visible rings stay as today.

### 4. Behaviour, `internal/web/static/app.js`

A small `rulesFilter()`, under 40 lines, bound when `.rules-bar` is
present:

- it reveals the bar;
- on `input` in the search box and on chip clicks, it toggles `hidden` on
  each `.rule-card`. A card stays visible when it matches the active chip
  (kind, or `on`/`off`/`hot`) and its `data-text` contains every search
  word;
- it shows `.rules-none` when nothing is visible;
- `clear-filter` resets both;
- the chips are single-select, with `aria-pressed`;
- the state is kept in `sessionStorage`, and only for the session.

It's bound on load and on `htmx:afterSwap`, because the switch form swaps
`#main`. No inline handlers, under the CSP.

## Alternatives rejected

- **Client-side "recently fired" with a timer.** The intent says server.
  A page that's already open shows the state as of its last render, and a
  toggle refreshes it.
- **Gemini's emoji pills.** The existing sprite icons are used instead,
  for consistency and accessibility.
- **The density toggle.** It's out of scope (intent).
- **Server-side filtering with query parameters.** It costs a round trip
  per keystroke. The list is small, and the client filter keeps working on
  a page that's already rendered.

## Risks

- **Card heights change** with the always-present footer, so the cards in
  a grid row line up better than today.
- **The JSON fields grow:** `/api/rules` consumers see new fields. That's
  additive.
- **CSP:** `TestTemplatesAreCSPClean` guards it.
- **Contrast** of the accent colours on `--surface`: check the hot pill and
  the hairlines in both themes, if a light theme exists. Look for
  `prefers-color-scheme` in `style.css`.
- **`role` checks:** the switch and Edit keep their existing `can` gates.

## Verification

- **`internal/web` tests:**
  - `/rules` renders `data-kind`, the provider badge for an agent rule, the
    footer pills (approval, for_each, repeat, cooldown) and `is-hot` for a
    rule fired 5 minutes ago with an injected clock, but not for one fired
    20 minutes ago. A disabled rule gets `is-off`;
  - the toolbar is present and `hidden`;
  - `TestTemplatesAreCSPClean` passes.
- **`go vet ./... && go test -race ./...`.**
- **Visual, on siphon-dev:** I take a screenshot before and after (Chrome
  tools), with the four existing rules, one fired recently through
  `./fire.sh`, and one disabled. Then I check:
  - search narrows the list, and the chips filter;
  - `/` focuses the search;
  - with reduced motion on, nothing animates;
  - a narrow window stacks the flow vertically.
