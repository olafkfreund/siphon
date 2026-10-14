---
status: draft
issue: 118
intent: intent/2026-10-14-118-portal-refresh.md
---

# Spec: portal refresh, slice 2: Approvals and Sources

This is the second slice (intent decision 1). It builds on slice 1's
shared primitives (`.item-card` and its states, `.item-head`/`-foot`,
`.stamp`, `.mode`, `.filter-bar` and `initFilters`). The input is a Gemini
4 Argon review (intent decision 2) at `scratchpad/agy-slice2.md`, checked
against the code.

## Findings checked in the code

**Approvals (`approvals.html`):**
- **The wrong anchor:** every row leads with the same "✋ Waiting for you"
  pill, an emoji that slice 1 removes portal-wide.
- **Missing context:**
  - the agent provider is dropped, because the row renders raw
    `{{$p.Kind}} {{$p.Target}}`, not `{{template "run" $p}}`;
  - there's no `#job` id;
  - there's no trigger (source);
  - the expiry is a bare wall-clock time.
- **Deny is unsafe:** it cancels the job, sits a few pixels from Approve,
  has no confirm, and is on a page that re-renders every 5 seconds, so the
  buttons can shift under the pointer.
- **Accessibility:** the "Approve" and "Deny" buttons have identical
  accessible names.
- **Dead class:** `.need-card` has no CSS.

**Sources (`sources.html`):**
- **An empty paragraph:** a healthy schedule source renders an empty
  `<p class="… mt">`.
- **Refusals look healthy:** a webhook that keeps refusing deliveries (a
  bad secret) shows a green dot and grey text. An expired OAuth login
  also looks healthy.
- **Hidden from viewers:** the "Awaiting Notion's verification token"
  status is inside an operator-only block, so viewers see nothing.
- **Colour alone:** the health dot carries health by colour only (its
  `title` doesn't count).
- **Unused hot state:** `sourceView.Hot` (slice 1) isn't used, and it
  ignores `LastRunAt` for schedules.
- **No identity or links:** `srcicon` has no schedule icon. There's no
  endpoint (the webhook URL), no Edit link, and no New source link. The
  empty state still says "add to siphon.yaml".

## Design

### Approvals

Each request is an `.item-card.is-wait` with `data-kind` for the action
kind, `data-filter-item` and `data-text` (rule, job, target, source).

| Part | Content |
|---|---|
| **Header** | the rule link, `#ID`, and a `.stamp.wait` with "waiting X · expires in Y". The stamp is `.stamp.err` when expiry is under 15 minutes. |
| **Body** | a two-node `.flow.flow-2`: the trigger (`srcicon`, source type, source name) → what will run (`{{template "run" $p}}`, with the provider, and the target in `<code>`) |
| **Footer** | the expiry time; Details; **Deny** (`.btn.sm.no` with the `i-x` icon); **Approve** (`.btn.sm.ok` with `i-check`) |

- **The decision buttons:**
  - each has an `aria-label`, such as "Deny job 104 (restart-nginx)";
  - **Deny gets `data-confirm="Deny job N? It will be cancelled."`,**
    using the existing handler at `app.js:47-50`. That's a safety
    addition: the page re-renders while you aim.
  - Approve has no confirm.
- **Viewers** see "an operator decides" where the buttons are.
- **The filter bar** (scope `approvals`) appears when there are two or
  more.
- **The forms stay plain POSTs** with the same `can` gates.
- **The view data:** the approvals' `ActionOf` entries gain `Source` and
  `SourceType` from the rule. The expiry-soon flag is computed in the
  template with `ExpiresAt` and the clock passed in the view, or a
  `Soon bool` set in Go. Go is simpler: `Soon = ExpiresAt - now < 15m`.

### Sources

Each source is an `.item-card` with these attributes:
- **`data-kind`** is the source type, and a new hairline colour per source
  kind: webhook `--ink`, mcp `--biolume`, schedule `--amber`, http
  `--current`;
- **`data-health`** and **`data-hot`**;
- **its state:** `.is-hot` when it's hot, `.is-err` on error, `.is-wait`
  when a webhook is refusing or an OAuth login has expired.

| Part | Content |
|---|---|
| **Header** | the dot, the name as a link to `/config/sources/<name>` (admin) or plain text, `srcicon` with the type, a **text** health `.stamp` (ok, stale, error, idle, or "refusing"/"login expired") so it's never colour alone, and Edit (admin) |
| **Body** | one `.node` with the source's endpoint (see the table below), the last error in `.blocked.is-err`, and the last refusal in `.blocked.is-wait`. Nothing empty is rendered. |
| **Footer** | `.mode` pills (`every 1m`, `tools only`, `N rules`), the OAuth login control (an outline button when the login is OK, primary when it's expired or missing), and the Notion verification. The status text shows to all roles; only the form is operator-only. |

**The endpoint in the body:**

| Type | Shows |
|---|---|
| webhook | `<public_url>/hook/<name>`, with a Copy button (`data-action="copy"`) |
| schedule | `at` and the timezone, next and last run |
| http and mcp | the URL, or "stdio: <package or command>", and the cadence |

- **The filter bar** (scope `sources`): kind chips (webhook, http, mcp,
  schedule) and health chips (error, stale, hot).
- **The top:** a New source link (admin).
- **The empty state** links to `/config/sources/new` and to templates.
- **The view data, `sourceView`:**
  - `Target string`: the webhook URL (using `server.public_url`, else the
    path only), the schedule's `at`, or the URL or stdio summary;
  - `Rules int`, the number of rules on it;
  - `Hot` also counts `LastRunAt`;
  - **health:** `Health = "stale"` when there's no error but a webhook is
    refusing (a refusal newer than the last event), or the OAuth login is
    expired. The Dashboard's counts and filter use the same field, so
    they stay consistent.

### Shared

- **`layout.html`:** `srcicon` gains `schedule` → `i-timer`. The `pill`
  template has no emoji (slice 1).
- **New CSS** (the only new classes):
  - `.flow.flow-2`, a two-column flow;
  - the source-kind hairlines on `.item-card[data-kind=webhook|mcp|schedule|http]`;
  - `.blocked.is-wait`;
  - `.mode .ico` (14px).

## Alternatives rejected

- **htmx approvals:** still out of scope, and still a behaviour change.
- **Showing the full command or prompt** on the approvals card: the job
  page has it. The card shows the target, which keeps it scannable and
  avoids leaking long prompts onto a shared screen.

## Risks

- **Health semantics:** "stale" now also means "refusing" or "login
  expired". The Dashboard's error count counts errors only, so it's
  unchanged. The stale count goes up for those cases, which is correct.
- **The Deny confirm** adds one click. It's justified by the moving page.
- **CSP, contrast and the polling pages,** as in slice 1.

## Verification

- **`internal/web` tests:**
  - **Approvals:** a card with `#ID`, the provider badge, the source,
    the `data-confirm` on Deny and the `aria-label`s; for a viewer, no
    forms and the note; the soon stamp under 15 minutes.
  - **Sources:**
    - a healthy schedule has no empty `<p>`;
    - a refusing webhook is `is-wait` and shows "refusing";
    - the webhook URL is shown;
    - a viewer sees the Notion status text;
    - the hot flag counts `LastRunAt`;
    - the rule count.
  - **CSP-clean.**
- **`go vet ./... && go test -race ./...`.**
- **Visual on siphon-dev:** both pages, the filters, both themes and a
  narrow window.
