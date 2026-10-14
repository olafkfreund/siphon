---
status: approved
issue: 115
spec: spec/2026-10-14-115-rules-page-refresh.md
---

# Plan: the Rules page is easy to scan, search and filter, and looks alive

## Approved decisions, carried over

- **"Hot"** means enabled and fired within 15 minutes, decided on the
  server with `s.Now()` (the injected clock, `web.go:60,97`).
- **The filter chips** are: All; the action kinds present (agent, cmd,
  unit, routine, servicenow); and the statuses enabled, disabled, fired
  recently. They're single-select. The search box matches the name,
  source, condition and target.
- **The page works without JS:**
  - the toolbar is rendered `hidden`, and `app.js` reveals it;
  - every card shows.
- **Reuse, don't invent:**
  - the icon templates `kindicon`, `srcicon` and `provider`
    (`layout.html:47-51`);
  - the `.dot`, `.chips`/`.chip` and `.badge` classes;
  - the palette tokens `--ink`, `--current`, `--biolume`, `--kelp`,
    `--amber`, `--coral`, `--line` and `--surface`;
  - no emoji.
- **Action-kind accents:** agent `--ink`, cmd `--current`, routine
  `--biolume`, unit `--amber`, servicenow `--kelp`. Disabled is `--line`.
- **Out of scope:** density mode, the routine editor (#116) and the YAML
  editor (#117).
- **Themes:** the app is light by default, with a dark theme via
  `prefers-color-scheme` (`style.css:17`). Both must keep contrast.

## Steps

1. **The view data, `internal/web/data.go`.**
   - **`ruleView` (`:76-89`)** gains `Approval bool`, `Provider string`,
     `ForEach bool`, `Repeat string`, `Egress bool` and `Hot bool`, with
     JSON tags `approval`, `provider`, `for_each`, `repeat` (omitempty),
     `egress` and `hot`.
   - **`rules()` (`:169-200`)** sets them:
     - `Approval` is `cfg.NeedsApproval(r)`;
     - `Provider` is `cfg.Agents[r.Action.Agent].Kind` when it's an agent
       action;
     - `ForEach` is `r.ForEach != ""`;
     - `Repeat` is the duration string when it's above 0;
     - `Egress` is `r.Egress.Enabled`;
     - `Hot` is `v.Enabled && v.LastFired != nil && s.Now().Sub(*v.LastFired) < 15*time.Minute`.
   - **The test, in `internal/web`:** a config with an agent rule
     (`approve`, `for_each`, `repeat`, `cooldown`), a cmd rule and a
     disabled rule, plus rule_state fire times through `store`. With an
     injected `Now`, `rules()` gives `Hot` for a fire 5 minutes ago and not
     for one 20 minutes ago, and the other fields as set.

   → verify: `go test -race ./internal/web -run Rules`.

   Traps: inject the clock, never real-time sleeps.

2. **The markup, `internal/web/templates/rules.html`.** Rewrite it per spec
   §2:
   - **The toolbar,** `div.rules-bar[hidden]`, holds the search
     (`input[type=search].rules-search`, with `aria-label`) and
     `button.chip[data-filter]`.
   - **The cards** get `data-kind`, `data-status`, `data-hot`,
     `data-text` and class `is-hot`/`is-off`.
   - **The header** is the dot, the name link, the override badge, `.sp`,
     the `.fired` pill, Edit, and the switch form, unchanged, with its
     `can` checks.
   - **The flow nodes** use the icon templates, the `.trig` pill and
     `.node-target` with a `title`. The provider shows for agents, and the
     `.sr` labels stay for screen readers.
   - **The footer,** `.rule-foot`, is always present.
   - **The empty state** links to `/help/templates` and to New rule (admin
     only).
   - **The filtered-empty message** is `p.rules-none[hidden]`, with
     `button[data-action=clear-filter]`.
   - **The test** extends step 1's: `/rules` HTML contains `data-kind="agent"`,
     the provider badge, the footer pills, `is-hot`, `is-off`,
     `class="rules-bar" hidden` (or the equivalent) and the empty-state
     links.

   → verify: `go test -race ./internal/web` (including
   `TestTemplatesAreCSPClean`, `csp_test.go:12`).

   Traps:
   - **No inline `style=` and no inline handlers.**
   - **Keep the role gates.** Check `roles_test.go:136-205`, which greps
     `/rules` for links.
   - **Keep `/config/rules/new` and `/config/rules/<name>`,** which
     `editors_test.go:141` greps for.

3. **The styles, `internal/web/static/style.css`,** in the rules block
   (`:131-150`):
   - **The `.flow` grid:** `minmax(6.5rem,.85fr) 30px minmax(10rem,1.5fr) 30px minmax(8rem,1.1fr)`.
     Keep the `@container` stacking, and the `flowx`/`flowy` pipes.
   - **`.node-target` and the source name** are single-line with an
     ellipsis. The condition `code` is clamped to 3 lines with
     `hyphens: none`. Remove the `overflow-wrap: anywhere` that split
     identifiers.
   - **The hairline:** `.rule-card::before`, 3px, from `data-kind`.
   - **`.is-hot`:** a tinted border, a soft glow, and the hot pill and
     pipes in `--biolume`.
   - **`.is-off`:** a dashed border, and `.dot.off` (hollow).
   - **New classes:** `.rule-foot`, `.rules-bar`, `.rules-search`,
     `.rules-none`, `.sr`, `.trig` and `.fired`.
   - **Hover:** a 1px lift, with `transition` on transform and
     border-color only.

   → verify: `go test -race ./internal/web` (embedded assets), plus a
   visual check in step 5.

   Traps:
   - The global `prefers-reduced-motion` rule (`:352`) already stops the
     animations. Don't add motion that isn't an animation or transition.
   - **Contrast in both themes.**

4. **The behaviour, `internal/web/static/app.js`.**
   - Add `rulesFilter()`, under 40 lines. It runs on `DOMContentLoaded`
     and on `htmx:afterSwap` (`:76`, which exists already).
   - **It does:**
     - reveal `.rules-bar`;
     - on search `input` and on chip click, set `hidden` on each
       `.rule-card` by chip (kind from `data-kind`, or `on`/`off`/`hot`)
       and every search word in `data-text`;
     - toggle `.rules-none`;
     - handle `clear-filter`;
     - set `aria-pressed` on the chips;
     - keep the state in `sessionStorage`, in try/catch.
   - The existing `/` shortcut (`:53-62`) needs no change.

   → verify: `go test -race ./internal/web`, and step 5 by hand.

   Traps: no `eval` or inline handlers, under the CSP.

5. **The visual check (mine, no repo edit).**
   1. Build to `siphon.new` and `mv` it over siphon-dev's binary. Back up
      `state.db` first, and stop and start the server by the PID from
      `ss`.
   2. Fire `ask-local` with `./fire.sh '{"kind":"echo","msg":"hi"}'` so
      `echo` is hot. Disable one rule in the portal.
   3. Take before and after screenshots with Chrome tools.
   4. Check:
      - search, the chips and `/`;
      - a narrow window (the flow stacks);
      - reduced motion (`--force-prefers-reduced-motion` or devtools
        emulation);
      - both colour schemes.
   5. Re-enable the rule afterwards.

## Deviations

- **Steps 2–4:**
  - **The kind chips:** all five are rendered, and the JS hides kinds with
    no cards. The template can't list the kinds present without a Go
    helper.
  - **`data-text`** is lower-cased by the JS, not the server.
  - **The condition:** clamped with `-webkit-box`, plus `.node { min-width: 0 }`.
  - **`.rule-card[hidden] { display: none }`** is added, because
    `display: flex` overrode `hidden`.
  - **Screen-reader labels** use the existing `.sr-only` class. The
    plan's new `.sr` duplicated it, so it's dropped.
  - **The egress pill** reads "egress limited".
- **Step 5, the visual check:**
  - the provider badge inherited the label's uppercase style and wrapped
    ("OPENAI-" / "COMPATIBLE"). `.node small .prov` resets it;
  - a `model` agent now shows its credential's provider (Ollama) instead
    of "OpenAI-compatible".

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** steps 1–4 edit 4 files, so the `coder` agent does them
one at a time. Step 5 is mine. A fresh Opus agent reviews `git diff`
against this plan.

## Rollback

Revert the PR. The view-data fields are additive, and nothing is stored.
