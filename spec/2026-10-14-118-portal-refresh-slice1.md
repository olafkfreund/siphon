---
status: approved
issue: 118
intent: intent/2026-10-14-118-portal-refresh.md
---

# Spec: portal refresh, slice 1: shared primitives, Dashboard, Jobs, job page

This is the first of the four slices (intent decision 1). It depends on
#115 (PR #120) being merged, because it generalises the classes #115
added.

**Input:** a Gemini 4 Argon design review of `dashboard.html`,
`jobs.html` and `job.html` (intent decision 2), checked against the code
below. Where I depart from it, the reason is given.

## Findings checked in the code

- **A real bug in the Dashboard's sources block** (`dashboard.html:21`).
  It shows `last delivery …` only from `.LastPollAt`. Webhook sources
  record deliveries in `.LastEventAt` (`data.go`) and never poll, so
  every webhook source permanently reads "waiting for a delivery".
  - **Schedule sources** never show their next run (`.NextRunAt`).
  - **Tool-only mcp sources** read "not polled yet" forever.
- **Emoji and symbols in `jobs.html`'s state chips:** `✋ ⟳ ◷ ✓ ✕ ⊘`. The ✋
  renders as a colour emoji, and none of the six is hidden from screen
  readers.
- **`job.html` splits one story across four cards:** Rule, Triggering
  event, What ran and Details. Only agent runs get the `run-ico`
  treatment, and `ul.plain` has no CSS.

## Design

### 1. Shared primitives

They're generalised from #115, in `static/style.css` and `static/app.js`.
In `rules.html` the rules-only class names are **renamed**, not aliased,
and the Rules page stays visually identical.

| #115 class | Shared class | Used for |
|---|---|---|
| `.rule-card` | `.item-card` | Any card for one thing: a rule, a job, a source. The `::before` hairline comes from `data-kind`. |
| `.rule-head` / `.rule-name` | `.item-head` / `.item-title` | The header row and the title link |
| `.rule-foot` | `.item-foot` | The always-present pill row |
| `.fired` (`.hot`) | `.stamp` (`.hot`, plus new `.err` and `.wait`) | Time, duration and count pills |
| `.trig` | `.mode` | Qualifier badges: each event, on edge, every 5m, attempt 2 |
| `.rules-bar` / `.rules-search` / `.rules-none` | `.filter-bar` / `.filter-search` / `.filter-none` | The search and chip toolbar, and the no-match state |

- **New state modifiers,** next to `.is-hot` and `.is-off`:
  - `.is-err`: a `--coral` tint, for a failed job or a source in error;
  - `.is-wait`: an `--amber` tint, for a pending approval.
- **The kind hairline** is unchanged from #115 (agent, cmd, routine, unit,
  servicenow). The state colours above apply where there's no kind.
- **The JS:** `rulesFilter()` becomes `initFilters()`.
  - **Each bar names its scope:** `.filter-bar[data-filter-scope=X]` goes
    with `[data-filter-list=X] [data-filter-item]` and a
    `.filter-none[data-filter-scope=X]`.
  - **Each item carries:**
    - `data-kind`;
    - `data-status` (on/off), `data-state` (a job state) or `data-health`
      (a source's health);
    - `data-hot`;
    - `data-text`.

    A chip matches kind, status, state, health or hot.
  - **The rest:** the state is kept per scope in `sessionStorage`, there's
    the bind-once guard, and it's bound on `DOMContentLoaded` and
    `htmx:afterSwap`.
  - **Polling pages** (the Dashboard and Jobs poll `#main`): the search
    keeps its focus and caret across a swap. They're saved on
    `htmx:beforeSwap` and restored after `initFilters`.

### 2. View data

| Where | Field | Value |
|---|---|---|
| `sourceView` (`data.go`) | `Hot bool` | the health is ok, and the last poll or last event is within 15 minutes (`hotWindow`) |
| the jobs list row (`pages.go`, the type `jobs.html` ranges over) | `Hot bool` | `State == "running"` |
| `jobView` (`pages.go:19`) | `Source`, `SourceType`, `When`, `On`, `Provider` | the rule's lineage, from the live config, with `Provider` resolved as `ruleView` does (#115) |

**Rejected from the review:** a failure `Summary` and `Duration` on every
row of the jobs list. `ListJobs` reads no output, so a summary would mean
reading the output of up to 200 jobs on every 5-second poll. Failed rows
show their state and exit code, and the summary stays on the job page.

### 3. Dashboard, `dashboard.html`

- **The subline:** "2 waiting for approval · 1 failed in 24 h · 4 sources"
  becomes three links: to `/approvals`, to `/jobs?state=failed`, and to
  `/sources`.
- **The four stat cards** become `.item-card.stat`:
  - "runs" is `is-hot` while there's a running job;
  - "success" is `is-err` when anything failed;
  - the agent cap bar gets `role="progressbar"` with `aria-valuenow`/`max`,
    and no `/ 0` when the cap is 0;
  - "sources" is `is-err` when one is in error.
- **"Needs attention":** each pending or failed job is an `.item-card`
  with `data-kind`, `is-wait` or `is-err`, the `#ID` and rule link,
  `{{template "run" .}}` and a `.stamp`.
  - The approve/deny forms are **unchanged** (plain POST, the same `can`
    gates). Turning them into htmx posts was rejected from the review:
    that's a behaviour change, and out of scope (intent).
- **Sources:** each row gets `srcicon`, a health dot and a `.stamp`. The
  status text fixes the bug:

  | Source | Text |
  |---|---|
  | webhook | "last delivery X ago" from `.LastEventAt`, else "waiting for a delivery" |
  | schedule | "next run …" from `.NextRunAt` |
  | tool-only mcp | "agent tools" |
  | polled | "polled X ago · every N" |

  An error still wins, as today.
- **Recent runs:** compact `.item-card` rows: the kind hairline, `is-hot`
  while running, the run, the state pill and the time, with a link to
  `/jobs`.

### 4. Jobs, `jobs.html`

- **The server-side state chips stay,** for deep links and no-JS use.
  `✋` becomes the sprite icon, matching `kindicon`'s use of `{{template
  "icon" …}}` with a hand or approval icon if one exists, otherwise none.
  The other glyphs get `aria-hidden="true"`, and zero counts are dimmed.
- **A new client `.filter-bar`** (`data-filter-scope="jobs"`) holds the
  search (id, rule, target, provider) and the kind chips plus "active".
- **The rows become compact one-line `.item-card.row`s,** not the review's
  two-column big cards. That keeps the 200-row list scannable:
  `#ID · rule · run (icon, target, provider) · state pill · attempt · time · exit`.
  - Each row carries the kind hairline (as a left border on rows),
    `is-hot` (running), `is-err` (failed), `is-wait` (pending) and
    `is-off` (cancelled).
  - Below 820px it stacks, as `table.cards` does today.

### 5. Job page, `job.html`

- **A hero `.item-card`** at the top shows the job's causal chain as the
  #115 `.flow`: source (`srcicon`, name) → rule (name, `.mode`
  each/edge) → action (`run`).
  - Its state modifier: `is-hot` running, `is-err` failed, `is-wait`
    pending, `is-off` cancelled.
  - Its `.item-foot` holds the stamps: created, started, finished,
    duration, exit, attempt, parent, depth.
- **The failure banner and Connect action stay** on top, unchanged.
- **What ran:** every kind gets the `run-ico` header, not only agents.
- **Routine steps:** the timeline gets the `.flowsteps` rail styling,
  plus `kindicon` per step.
- **The triggering event** gets a Copy button (the existing copy pattern
  in `app.js`, if there is one; otherwise a small addition).
- **`ul.plain`** gets CSS.

## Alternatives rejected

- **Aliasing the old `.rule-*` classes:** two names for one thing. The
  rename is mechanical.
- **Big two-column job cards:** too sparse for 200 rows.
- **Summaries on the list:** an output read per row on every poll.
- **htmx approvals:** a behaviour change, out of scope.

## Risks

- **Polling pages re-render `#main` every 5 seconds.** The filter state
  and the search focus must survive. That's handled by the scope state
  plus saving the caret.
- **Tests that grep the pages** (`pages_test.go`, `roles_test.go`,
  `editors_test.go`, the dashboard and jobs tests) must keep passing. The
  routes, links and role gates are unchanged.
- **CSP:** `TestTemplatesAreCSPClean`.
- **Contrast** in light and dark: the new `.is-err` and `.is-wait` tints.

## Verification

- **`internal/web` tests:**
  - **The Dashboard:** a webhook source with an event shows "last
    delivery". This is the regression test for the bug. A schedule source
    shows "next run".
  - **Jobs:** no emoji in the markup, the `data-state` and `is-err` on a
    failed row, and the filter bar present and hidden.
  - **The job page:** the hero flow carries the source, the rule and the
    action.
  - **Rules:** still renders, with the renamed classes.
  - `TestTemplatesAreCSPClean`.
- **`go vet ./... && go test -race ./...`.**
- **Visual, on siphon-dev** (once you're signed in):
  - before and after screenshots of the Dashboard, Jobs and a job;
  - the filters, and `/`;
  - the search focus surviving a 5-second poll;
  - a narrow window;
  - both themes.
