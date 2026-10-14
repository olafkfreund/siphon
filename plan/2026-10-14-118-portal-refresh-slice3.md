---
status: draft
issue: 118
spec: spec/2026-10-14-118-portal-refresh-slice3.md
---

# Plan: portal refresh, slice 3: config lists, Services, Connections

## Approved decisions, carried over

- **Four verified bugs are fixed:**
  1. the Services hidden `cat` moves inside `#service-results`, with
     `form=`;
  2. the Connections Test target is a relative `div.conn-test`, not an id
     built from the name;
  3. the login radio: a CSS exclusion for agy plus apikey, and the JS
     re-checks `login` when the checked `kind` is hidden;
  4. the global `.g3` override (`style.css:482`) is removed, and scoped to
     `.helplayout` if needed.
- **`cfglist`:**
  - the kind nav is `.chips`;
  - rows are `.item-card.row` with a filter bar when there are more than
    6;
  - Reset gets `data-confirm`;
  - **agents** add the provider, model, credential, MCP, egress and rule
    count;
  - **routines** add `.steps-mini` kind chips, approval and the rule
    count;
  - the empty state links to New and to templates.
  - `cfgRow` gains these fields for agents and routines only.
- **Services:**
  - `.chips` categories;
  - availability pills with icon plus text;
  - Connect for admins only;
  - connected rows as `.item-card`;
  - `connRow.LastEventAt`, with a relative time, and "agent tools" for
    tools-only services.
- **Connections:**
  - `.item-card` with `.is-err`/`.is-ok`, a dot plus text, and "used by N
    agents";
  - add controls for admins only;
  - `loginView`/model `Agents []string`.
- **New CSS:** `.item-card.is-ok::before`, `.conn-test` and `.steps-mini`.
- **No behaviour change:** the same routes, forms and gates. The edit
  popover keeps its behaviour.

## Steps

1. **The view data:** `cfgRow` fields (`configweb.go`),
   `connRow.LastEventAt` (`servicespage.go`), and login and model
   `Agents` (`pages.go` or the logins view).
   - **The tests:** the agents and routines rows, the login agents, and
     `LastEventAt`.

   → verify: `go test -race ./internal/web`.

2. **`config.html` `cfglist`,** plus CSS.
   - **The tests:** per the spec, the config lists.

   → verify: `go test -race ./internal/web`.

3. **`services.html`,** with bug 1.
   - **The tests:** per the spec, Services.

   → verify: `go test -race ./internal/web`.

4. **`logins.html`, `app.js` and `style.css`,** with bugs 2, 3 and 4.
   - **The tests:** per the spec, Connections and `.g3`.

   → verify: `go test -race ./internal/web`.

5. **The visual check (mine).**

## Tests

```sh
go vet ./... && go test -race ./...
```

**Model split:** the `coder` agent does steps 1–4. A fresh Opus agent
reviews.

## Rollback

Revert the PR.
