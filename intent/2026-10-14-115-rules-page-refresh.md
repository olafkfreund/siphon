---
status: draft
issue: 115
author: olafkfreund
---

# Intent: the Rules page is easy to scan, search and filter, and looks alive

## Problem

The Rules list, `internal/web/templates/rules.html`, shows every rule as an
identical card. A design review (Gemini 4 Argon, via `agy`, checked against
the code) found the concrete causes:

- **Nothing tells rules apart.**
  - `layout.html` already has per-kind icons (`srcicon`, `kindicon`,
    `provider`), and `style.css` has per-kind colour tokens (`.k-agent`,
    `.k-cmd`, `.k-unit`, `.k-routine`) and status dots (`.dot.ok`, `.idle`,
    `.error`).
  - `rules.html` uses none of them. A rule that runs an AI agent looks the
    same as one that runs `echo`.
- **The text is cramped and breaks badly.** The flow is three equal flex
  boxes in a half-width card. The condition and the target get about 130px,
  and `overflow-wrap: anywhere` breaks identifiers mid-word (`local-` /
  `helper`).
- **The label noise.** Every card repeats uppercase `SOURCE ·` and `WHEN ·`
  labels, which take the width the content needs.
- **Activity is invisible.** "Fired 30 seconds ago" and "fired 7 Oct" look
  the same. The muted timestamp sits between the Edit button and the
  toggle, so Edit moves with its length.
- **The settings that matter are hidden:** approval required, `for_each`,
  `repeat`, egress. Only `cooldown` shows, as a bare line that's missing on
  rules without one, so the cards don't line up.
- **No way to find a rule.** There's no search or filter, although
  `app.js` already binds `/` to a search box.

## Proposed outcome

- **Each card shows what kind of rule it is at a glance:**
  - a source icon and an accent per source kind (webhook, http, mcp,
    schedule);
  - an icon and an accent per action kind (agent, cmd, unit, routine,
    servicenow), with the provider badge for agents.
- **The flow fits its content.** The condition gets the most room,
  identifiers never break mid-word (a long target ends in an ellipsis,
  with the full text on hover), and the repeated label words go.
- **Recent activity stands out.**
  - A rule that fired recently gets a subtle glow, a highlighted
    fired-time pill, and flow arrows that move. The arrows only move when
    the user hasn't asked for reduced motion.
  - A disabled rule looks visibly off.
  - The header reads: status dot and name, then fired time, Edit and the
    toggle, in a stable order.
- **A consistent card footer** shows the rule's key settings as small
  pills: cooldown, asks for approval, `for_each`, repeat, egress.
- **A search box and filter chips** (by action kind and by
  enabled/disabled/recently-fired) narrow the list instantly, with `/`
  focusing the search. A "no rules match" state has a reset.
- **The empty state** points to templates and to New rule.

## Affected users and systems

- Everyone using the portal's Rules page.
- `internal/web/templates/rules.html`, `internal/web/static/style.css`,
  `internal/web/static/app.js`, and the rule view data in
  `internal/web/data.go` (`ruleView`), which needs a few more fields:
  approval, provider, `for_each` and repeat, and a "recently fired" flag.

## Constraints

- **The strict CSP holds:** no inline `style=` or `<script>`
  (`TestTemplatesAreCSPClean`).
- **No new JS framework, build step or CDN asset.** The filtering is a
  small addition to `app.js`, and the page still works without JS. Then
  every rule shows, and the search and chips are hidden.
- **Accessible:** contrast, focus rings and `prefers-reduced-motion`. Any
  animation is decoration only.
- **"Recently fired" is decided on the server,** so there's no client
  timer.
- **Reuse the existing icons and colour tokens.** No emoji.
- **Out of scope:**
  - the density toggle (comfort/compact), until someone has enough rules
    to need it;
  - the routine editor (#116) and the YAML editor (#117).

## Open questions

1. **What "recently fired" means.** I'd say within the last 15 minutes:
   long enough to notice after a test, short enough to mean "live". OK?
2. **What the filter chips filter.** I'd filter by action kind (agent,
   cmd, unit, routine, servicenow) and status (enabled, disabled, fired
   recently). Filtering by source kind is left out, because the search box
   already matches the source name. OK?
