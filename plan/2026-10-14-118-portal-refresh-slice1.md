---
status: approved
issue: 118
spec: spec/2026-10-14-118-portal-refresh-slice1.md
---

# Plan: portal refresh, slice 1: shared primitives, Dashboard, Jobs, job page

## Approved decisions, carried over

- **It's built on #115 (PR #120).** Before step 1, rebase or merge
  `origin/main` once #120 has merged. Until then, base on
  `feat/115-rules-page-refresh`.
- **The shared classes rename the #115 ones,** with no aliases:

  | #115 | Shared |
  |---|---|
  | `.rule-card` | `.item-card` |
  | `.rule-head` | `.item-head` |
  | `.rule-name` | `.item-title` |
  | `.rule-foot` | `.item-foot` |
  | `.fired` | `.stamp` (`.hot`, `.err`, `.wait`) |
  | `.trig` | `.mode` |
  | `.rules-bar` | `.filter-bar` |
  | `.rules-search` | `.filter-search` |
  | `.rules-none` | `.filter-none` |

  New: `.is-err` (`--coral`) and `.is-wait` (`--amber`). The Rules page
  stays visually identical.
- **`initFilters()`** replaces `rulesFilter()`:
  - `data-filter-scope` on the bar, `data-filter-list`/`data-filter-item`
    on the list and its items;
  - the attributes `data-kind`, `data-status`, `data-state`,
    `data-health`, `data-hot` and `data-text`;
  - the state is kept per scope in `sessionStorage`, with the bind-once
    guard;
  - the search's focus and caret are saved on `htmx:beforeSwap` and
    restored after a swap.
- **The view data:**
  - `sourceView.Hot`: the health is ok and the last poll or last event is
    within `hotWindow`;
  - `jobView`: `Source`, `SourceType`, `When`, `On` and `Provider`, from
    the live config.
  - A provider helper is extracted from #115's `rules()` (agent kind;
    model gives the credential's `Preset`, else its `Provider`), and used
    by `rules()`, `runs()` (`pages.go:261`) and `jobView`.
  - **Deviation from the spec:** no `Hot` field on the jobs list. The
    template tests `eq .State "running"`.
- **The Dashboard sources fix:**

  | Source | Text |
  |---|---|
  | webhook | `.LastEventAt` ("last delivery X ago"), else "waiting for a delivery" |
  | schedule | `.NextRunAt` ("next run …") |
  | tool-only mcp | "agent tools" |
  | polled | "polled X ago · every N" |

  An error still wins.
- **Jobs:** the server-side state chips stay. Their glyphs become sprite
  icons: `i-hand` waiting, `i-play` running, `i-timer` queued, `i-check`
  done, `i-x` failed, and none for cancelled. All are `aria-hidden`, and
  zero counts are dimmed. The rows are compact one-line `.item-card.row`s.
- **No behaviour change:** the approve/deny forms stay plain POSTs, and
  every route, link and role gate is the same.
- **Constraints:**
  - CSP-clean, with no new library and no emoji;
  - works without JS;
  - reduced motion respected;
  - contrast holds in light and dark.

## Steps

1. **The shared primitives.**
   - **`static/style.css`:** rename the #115 classes, and add `.is-err`,
     `.is-wait`, `.stamp.err`, `.stamp.wait` and `.item-card.row`. The
     row is a one-line grid with a left kind border instead of the top
     hairline, stacked below 820px.
   - **`static/app.js`:** generalise `rulesFilter` into `initFilters`,
     with the focus and caret kept across swaps.
   - **`templates/rules.html`:** the renamed classes and the
     `data-filter-*` attributes.
   - **The tests:** update `TestRulesPageMarkup` to the new class names.

   → verify: `go test -race ./internal/web`, plus a visual check that the
   Rules page is unchanged.

   Traps:
   - The CSP.
   - **The `[hidden]` override** for `.item-card` must come after its
     `display` rule.
   - The bind-once guard is per bar.

2. **The view data.**
   - **`data.go`:** add `sourceView.Hot`, and the `providerOf(cfg, agentName)`
     helper used by `rules()`.
   - **`pages.go`:** `runs()` uses `providerOf`, and `jobView` gains the
     lineage fields, set where `jobView` is built.
   - **The tests:**
     - `Hot` for a webhook with an event 5 minutes ago, and not 20
       minutes ago;
     - `jobView` lineage for a job of a known rule;
     - a model agent's provider in `runs()`.

   → verify: `go test -race ./internal/web`.

3. **`templates/dashboard.html`,** per the spec §3:
   - the subline links;
   - the `.item-card.stat` cards, with an accessible progress bar and no
     `/ 0`;
   - "Needs attention" as `.item-card`s with the forms unchanged;
   - the Sources rows with `srcicon`, a dot, a `.stamp` and **the status
     fix**;
   - Recent runs as `.item-card.row`s.
   - **The tests:**
     - **the regression test:** a webhook source with an event shows
       "last delivery";
     - a schedule source shows "next run";
     - the approve and deny forms are still present for an operator.

   → verify: `go test -race ./internal/web`.

   Traps: the Dashboard polls `#main` every 5 seconds, so don't break
   `hx-` attributes on it.

4. **`templates/jobs.html`,** per the spec §4:
   - the chip glyphs become sprite icons;
   - a `.filter-bar[data-filter-scope=jobs]`;
   - `.item-card.row` rows with `data-kind`, `data-state`, `data-text`
     and the state modifiers.
   - **The tests:**
     - no `✋⟳◷✓✕⊘` in the markup;
     - a failed row has `is-err` and `data-state="failed"`;
     - the bar is present and `hidden`.

   → verify: `go test -race ./internal/web`.

5. **`templates/job.html`,** per the spec §5:
   - the hero `.item-card` with the source → rule → action `.flow` and
     the stamps;
   - every kind gets the `run-ico`;
   - the steps timeline gets the `.flowsteps`-style rail and `kindicon`;
   - the event's Copy button reuses `data-action="copy"` (`app.js:24`);
   - `ul.plain` gets CSS.
   - **The test:** the job page shows the hero flow with the rule's
     source name and the action target.

   → verify: `go test -race ./internal/web`.

6. **The visual check (mine).**
   - Deploy to siphon-dev: back up the database, and restart by PID. With
     #119 merged by then, you stay signed in.
   - Take before and after screenshots of the Dashboard, Jobs, a job and
     Rules (unchanged).
   - Check:
     - the filters and `/`;
     - the search focus across a 5-second poll;
     - a narrow window;
     - both themes.

## Deviations

- **Step 1:**
  - **`sources.html`** also used `rule-head`, so it's renamed to
    `item-head`.
  - **`.is-err` and `.is-wait`** use the theme-aware `--bad-text` and
    `--wait-text` tokens, so the contrast holds in dark mode.
  - **`.stamp.err` and `.stamp.wait`** follow `.stamp.hot`.
  - **A chip is hidden** when no item has its value in any filter
    attribute. `all`, `hot`, `on` and `off` always show.
- **Steps 2–5:**
  1. **The shared `pill` template** (`layout.html`) printed the emoji in
     every state pill, portal-wide. It now uses sprite icons: `timer`,
     `hand`, `check` and `x`, none for cancelled, and the running pill
     keeps its CSS bubbles. The topbar's approvals pill is changed the
     same way.
  2. **`.item-card.row`** is a flex row of fixed-basis cells (`.c-id`,
     `.c-rule`, `.c-run`, `.c-state`, `.c-att`, `.c-time`, `.c-exit`), so
     the columns line up. It wraps below 820px.
  3. **No "active" chip** on Jobs: the chips match `data-kind` and
     `data-state` exactly, and the server-side state chips already cover
     running.
  4. **The job page's Details card** is folded into the hero's stamps.
  5. **A shared `jobrow` template** (in `jobs.html`) is reused by the
     Dashboard's recent runs.
- **The Opus review:** no HIGH, and two MEDIUM, both fixed.
  - **M1:** real model-agent jobs carry an agent snapshot with kind
    `model`, so `runs()` never resolved the provider, and Jobs and the
    Dashboard showed "OpenAI-compatible".
    - `store.JobAction` now reads the snapshot's `Credential`.
    - `runs()` resolves `model` through it (`modelProvider`).
    - The test uses a real snapshot payload, for an agent that has since
      been deleted.
  - **M2:** the job page now takes the provider from the job's snapshot
    (`agentProvider(v.Agent, …)`), not the live config. The config is
    read once per lookup.
  - **LOW, fixed:**
    - reduced motion also stops `::before` animations;
    - the topbar approvals pill has an `aria-label`;
    - job rows have an `exit` label for screen readers;
    - a deleted rule on the job page is plain text, not a dead link;
    - `.stamp.err` uses a 12% tint, for dark-mode contrast;
    - the filter keeps the chosen chip across polls that leave it
      matching nothing;
    - `TestSourceHot` checks that its source exists.
  - **LOW, accepted (per the spec):** the lineage comes from the live rule
    by name.

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** steps 1–5 edit files, so the `coder` agent does them one
at a time. Step 6 is mine. A fresh Opus agent reviews `git diff` against
this plan.

## Rollback

Revert the PR. The rename is self-contained in the three static or
template files plus their tests, and the view data is additive.
