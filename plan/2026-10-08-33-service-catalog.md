---
status: approved
issue: 33
spec: spec/2026-10-08-33-service-catalog.md
---

# Plan: A service catalogue, and a redesigned Services page

## Approved decisions (self-contained)

**Evidence:**
- [`research/2026-10-08-33-service-catalog.md`](../research/2026-10-08-33-service-catalog.md):
  each service's webhook scheme, MCP auth and token;
- [`research/2026-10-08-33-services-design-review-codex.md`](../research/2026-10-08-33-services-design-review-codex.md):
  the page design.

### The catalogue

`internal/catalog/services.yaml` is embedded and is the single source of
truth. Each entry has:
- `id`, `name`, `mark` (a 2-letter monogram; no vendor logos), `category`
  (code | issues | chat | monitoring | cloud | payments | home | generic),
  `summary` and `capabilities` (tools | polling | webhooks);
- `status` (available | needs-package | not-yet) and `reason`;
- `fields` (key, label, type text|secret|url|choice|bool, default, help,
  required, choices);
- `creates`: a list of overlay items `{kind, name (template), when?, yaml
  (template)}`;
- `setup`: provider-side steps, a template;
- `test`: `{method, url, header, body, identity (JSON path)}`;
- `templates`: template names;
- `hooks`: an optional Go hook, used only for `aws`.

**Template functions** (`text/template`):
- `secret "field" "fmt"`: a write-only secret through the existing
  `pendingSecret` path, written as a `file:` ref;
- `basic "emailField" "tokenField"`: `Basic base64(email:token)`, as a
  secret;
- `generated "secret"`: a random webhook secret, shown once.

A field's raw value is **never** inserted into YAML except through these
functions, or as validated non-secret text (name, URL, project).

### The engine and the label

`catalog.Connect(entry, values, actor)` builds the items, the pending
secrets and the done-info, then makes one `commit` (one revision, through
`putItems`), so `checkOverlay`, `credsCheck` and the audit apply.

Every created item carries the additive `connection: <name>` field (on
sources and credentials; metadata only, validated
`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`). Connected groups by this label.
Unlabelled legacy items keep today's heuristic grouping.

GitHub, GitLab and AWS become catalogue entries. The old functions are
removed, and the old portal and API routes stay as aliases.

### Webhook verification

- **`slack`:** `v0=` + hex HMAC-SHA256 of `"v0:" + ts + ":" + body`.
  The timestamp is in `X-Slack-Request-Timestamp` and the signature in
  `X-Slack-Signature`; the window is ±300 s.
- **`stripe`:** parse `t` and the `v1` values from `Stripe-Signature`;
  hex HMAC-SHA256 of `t + "." + body`; any `v1` may match; ±300 s.
- **`sha256` gains:**
  - `signature_prefix`: e.g. `v1=`; the header may hold a
    comma-separated list, and any one may match;
  - `timestamp_separator`: default `.`; `:` for Grafana.

All of them are constant-time. Tests use the documented algorithms with
fixed vectors.

### API

- `GET /api/catalog`: the entries without templates, with availability on
  this install. `needs-package` is computed from `server.mcp_packages`.
- `POST /api/services/{id}` `{fields}`: returns the done-info, with the
  one-time secret and `no-store`.
- `POST /api/connections/{name}/test`: stores the result in a new
  `connection_check(connection PRIMARY KEY, at, ok, detail)` table
  (migration **0004**).
- The old `/api/services/github|gitlab|aws` routes stay as aliases.

### Portal (following the Codex review)

- **`/services`:**
  - **Connected first:** one row per connection, with the monogram,
    `Name · name`, capability chips, a status pill (Not checked / Working /
    Needs attention, from `connection_check`), the last event (from its
    sources' `source_state`), Test where there's a test, and Edit.
  - **Then Explore:** search (`?q=`), category links (`?cat=`), and a tile
    grid (min 240 px, 36 px monogram, summary, capability chips, an
    availability pill, Connect). Unavailable tiles show the reason and
    have no Connect. There's an empty state. htmx swaps the grid, and
    plain links work without JavaScript.
- **`/services/{id}`** (max 640 px): numbered sections (1. What to connect,
  2. Access, 3. Events). The webhook toggle is one aligned row. Errors
  appear inline and keep the values typed. The primary button is at the
  end, with Back beside it.
- **The done page** shows the one-time secret, the `setup` steps, and the
  tools to allow.
- CSP-clean, light and dark, usable on a phone.

### CLI

- `siphon catalog [--category c] [-o json]`.
- `siphon connect <id> --<field> value …`: secrets only via `-` or
  `@file`.
- The old `connect github|gitlab|aws` flags are kept as aliases.
- `siphon draft` lists available services from the catalogue in its
  prompt.

### Nix

- `services.siphon.catalogPackages`: catalogue IDs (`grafana`, `gitea`,
  `forgejo`, `kubernetes`) whose pinned MCP servers are added at
  option-default priority.
- Their env names and hosts come from `nix/catalog-packages.json`,
  produced by `go generate ./internal/catalog`, with a freshness test.

### The first wave

| Category | Services |
|---|---|
| code | `github`, `gitlab` (tools not-yet: OAuth), `bitbucket` (Rovo, `basic`), `gitea`, `forgejo` |
| issues | `linear`, `jira` |
| chat | `slack` (webhooks only) |
| monitoring | `sentry` (webhooks; tools need packaging), `pagerduty`, `grafana`, `alertmanager`, `datadog`, `uptime-kuma` |
| cloud | `aws`, `cloudflare`, `kubernetes` |
| payments | `stripe` (Agent key) |
| home | `home-assistant`, `ntfy` |
| generic | `webhook`, `mcp` |

- **Listed as not-yet, each with its reason:** discord, teams,
  mattermost, matrix, notion, google-pubsub, azure-eventgrid,
  azure-devops, opsgenie.
- **Research items marked "unverified"** (PagerDuty's MCP header, the
  Datadog MCP URL) start as `not-yet` for tools, with the reason
  "unverified".

## Steps

Each step is one commit, citing "Plan step N". Go steps: `go vet ./...`
and `go test -race ./...`. After `go.mod` or migration changes, also
`nix build --rebuild .#siphon.goModules` and `nix flake check`.

| # | Step | Who | Main files |
|---|---|---|---|
| 1 | Catalogue package: the schema, loader, template engine (`secret`/`basic`/`generated`), `Connect`, the `connection:` label; github, gitlab and aws as entries; the old preset functions removed; every-entry validation test | coder | `internal/catalog/`, `internal/config/config.go`, `internal/web/services.go` |
| 2 | Webhook modes: `slack`, `stripe`, `sha256` prefix/list and separator | coder | `internal/source/webhook.go`, `internal/config/config.go` |
| 3 | API: `/api/catalog`, `POST /api/services/{id}`, connection test plus `connection_check` (migration 0004), aliases | coder | `internal/web/connapi.go`, `internal/store/migrations/0004_*.sql` |
| 4 | Portal redesign: Connected grouping, Explore (search and filters), the connect page, the done page, CSS | coder | `internal/web/services.go`, `templates/services*.html`, `static/style.css`, `static/app.js` |
| 5 | CLI: `catalog`, `connect <id>`, the aliases, the draft catalogue list | coder | `cmd/siphon/`, `internal/draft/` |
| 6 | The first-wave entries, each with a connect test against a fake server; `catalogPackages` in Nix, the JSON generator and the eval check | coder (Go) + Opus (Nix) | `internal/catalog/services.yaml`, `nix/module.nix`, `flake.nix` |
| 7 | Templates for the new services; the connections index generated from the catalogue; docs | Opus | `docs/` |
| 8 | VM subtest: connect `linear` against a stub, and a `slack`-mode webhook delivery | Opus | `nix/vm-test.nix` |
| 9 | Review (fresh Opus), screenshots, owner live, PR | Opus | none |

**Traps:**
- `TestTemplatesAreCSPClean` must pass.
- No sentinel secret may appear in any rendered YAML; assert it for every
  entry.
- Use `sockDir` for any socket tests.
- Run `nix build --rebuild` after `go.mod` changes.
- Regenerate `docs/cli.md` (`go generate ./docs`) when CLI flags change.
- Migration 0004 must apply cleanly on a DB from `main`.
- #31 (the schedule source) touches `config.go` and the web code too;
  rebase or merge carefully when it lands.

## Tests

`go test -race ./...`, `devenv test`, `nix flake check` (the VM test with
the new subtest, plus the eval check), the docs tests, CI green, light and
dark screenshots, and the owner's live check: one real new service
connected, and seen as **Working** after Test.

## Handoff

- **Coder:** steps 1–6 (Go), one agent, steps in order via SendMessage.
- **Opus:** the Nix part of step 6, and steps 7–9.

## Rollback

Revert the merge.
- **Migration 0004:** only adds a table.
- **The `connection:` label:** additive.
- **Old routes and flags:** remain as aliases.

## Deviations log
- **Step 1 (coder):**
  - **Template funcs:** `secret "field" "yaml.path"`, `basic "email" "token" "path"` and `generated "path"` take the config path, because secret files are tied to their field path (#29's injective naming). A value prefix (e.g. `Bearer `) is added in step 6, when Linear needs it.
  - **Engine and commit:** the engine is `Entry.Render(values, Env)` → items, secrets and done-info. The commit is `server.connect` in `internal/web`, which avoids an import cycle.
  - **Extras:** more funcs (`q`, `pathesc`, `has`, `package`, `private`, `fail`, `tools`), a `multi` field type, field `pattern`/`secure`, and `hook_header`.
  - Required-field errors read "<label> is required".
  - Grouping by `connection:` is in step 4.
- **Step 3 (coder):**
  - Entries declare `needs_package`, and `Entry.Availability(cfg)` derives the status. `Render` refuses unavailable entries.
  - Test templates use `{token}`/`{base}` substitution, in memory.
  - The test runner (`runHTTPTest`) is shared by the old and new endpoints. `Err`, `User` and `ARN` are masked before they're stored or returned; the masking test caught a token echoed in `user`.
  - **Decision (Opus):** items also get an additive `service: <catalogue id>` field next to `connection:`. The test lookup and the Connected grouping use it instead of the source-shape heuristic, which only knew github, gitlab and aws. Added in step 4.
