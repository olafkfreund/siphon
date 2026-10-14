---
status: draft
issue: 118
author: olafkfreund
---

# Intent: the whole portal shares the Rules page's design language

## Problem

The Rules page refresh (#115) made one page easy to scan and visibly
alive:
- **Kind at a glance:** per-kind icons and an accent hairline, reusing the
  existing `srcicon`, `kindicon` and `provider` templates and the palette
  tokens.
- **Status:** a status dot, plus a glow for recent activity.
- **Settings:** a consistent card footer of setting pills.
- **Finding things:** a client-side search with filter chips, and clear
  empty and no-match states.

The owner's verdict: "looks much better", so do the same for the whole
portal.

The rest of the portal still has the old look. Plain cards and tables
carry no kind or status cues, and nothing shows what's live or what's
failing at a glance. There's no consistent way to find things in a long
list. The visual vocabulary also differs between pages.

The design pieces from #115 are written as rules-only classes
(`.rule-card`, `.rule-foot`, `.rules-bar`, …). Copying them page by page
would fork the stylesheet.

## Proposed outcome

- **One shared set of portal primitives,** in `style.css` and `app.js`,
  generalised from #115 and used by every page:
  - a card accent by `data-kind`;
  - `is-hot`, `is-off` and `is-error` states, with dot and glow;
  - time and status pills;
  - a footer row of setting pills;
  - a filter bar with search and chips, client-side, and hidden without
    JS;
  - empty and no-match states.

  The Rules page moves onto them, with no visual change.
- **Every main page uses them,** with each page getting what helps it
  most:

  | Page | What it gets |
  |---|---|
  | **Dashboard** | at-a-glance health: what fired, what's failing, what waits for approval |
  | **Jobs and job** | kind icons, state colours, search and filter by state, kind and rule |
  | **Approvals** | waiting items stand out, with the action kind and the requester clear |
  | **Sources** | per-source-kind accents, health dots, last event and last error, filter |
  | **Agents, Routines and the config lists** | kind and provider badges |
  | **Services and Connections** | status and capability pills, and the existing catalogue filter aligned |
  | **Notifications, Egress, Audit and History** | consistent tables, pills and empty states |
  | **Help** | a light touch only |

- **It ships in slices,** one PR per group of pages, so each one is
  reviewable and shows something visible. The shared primitives come
  first.

## Affected users and systems

- Everyone using the portal.
- `internal/web/templates/*.html`, `internal/web/static/style.css`, and
  `internal/web/static/app.js`.
- Possibly small additions to the view data in `internal/web/*.go`, where
  a page needs a kind or status it doesn't have yet (as #115 did for
  rules).

## Constraints

- **The same as #115:**
  - the strict CSP (no inline `style=` or `<script>`,
    `TestTemplatesAreCSPClean`);
  - no new framework, build step or CDN asset;
  - works without JS;
  - `prefers-reduced-motion` respected, and contrast holds in light and
    dark;
  - the existing icons and tokens reused, and no emoji.
- **No behaviour change** beyond presentation and client-side filtering:
  - the same routes, forms and role gates;
  - the existing tests that grep pages keep passing.
- **It depends on #115 merging,** because it generalises #115's classes.
- **Out of scope:**
  - the editors (#116, #117);
  - the session-key fix (#119);
  - any new feature behind a page.

## Open questions

1. **Slicing.** I'd use four slices:
   1. the shared primitives, with Rules moved onto them, Dashboard and
      Jobs;
   2. Approvals and Sources;
   3. Agents, Routines, Services and Connections;
   4. Notifications, Egress, Audit, History and Help.

   OK, or another order?
2. **A design review first?** Should `agy` (Gemini) review the other
   pages first, as it did for Rules, so each slice's spec starts from
   concrete critique? I'd do it per slice, just before its spec.
