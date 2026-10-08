---
status: approved
issue: 33
intent: intent/2026-10-08-33-service-catalog.md
---

# Spec: A service catalogue, and a redesigned Services page

## Design

### 1. The catalogue: one data file

`internal/catalog/services.yaml` is embedded in the binary and is the
single source of truth. One entry per service:

```yaml
- id: linear
  name: Linear
  mark: LN                       # 2-letter monogram, no vendor logo
  category: issues               # code | issues | chat | monitoring | cloud | payments | home | generic
  summary: Issues and projects: agents create and triage issues; events by webhook.
  capabilities: [tools, webhooks] # tools | polling | webhooks
  status: available              # available | needs-package | not-yet
  reason: ""                     # shown when not available, e.g. "MCP needs OAuth"
  fields:
    - { key: name,  label: Name,    type: text,   default: linear }
    - { key: token, label: API key, type: secret, help: "Settings → Security & access → Personal API keys. Read-only if your plan allows." }
    - { key: hook,  label: Also receive webhooks, type: bool, default: true }
  creates:                       # overlay items, rendered with text/template over the field values
    - kind: sources
      name: "{{.name}}"
      yaml: |
        type: mcp
        url: https://mcp.linear.app/mcp
        headers: { Authorization: "{{secret "token" "Bearer %s"}}" }
    - kind: sources
      name: "{{.name}}-hooks"
      when: hook
      yaml: |
        type: webhook
        signature: sha256
        signature_header: Linear-Signature
        secret: "{{generated "secret"}}"
  setup: |                       # provider-side steps, shown on the done page
    In Linear: Settings → API → Webhooks → New webhook. URL: {{.hook_url}}. Secret: {{.hook_secret}}.
  test: { method: POST, url: https://api.linear.app/graphql, header: { Authorization: "{{secret "token" "%s"}}" },
          body: '{"query":"{ viewer { name } }"}', identity: data.viewer.name }
  templates: [linear-issue-from-alert]
```

**Template functions:**
- `secret "field" "format"`: the value becomes a write-only secret
  through the existing `pendingSecret` path (format applied, e.g.
  `Bearer %s`). The YAML gets its `file:` ref.
- `generated "secret"`: a new random webhook secret, shown once.
- `basic "email" "token"`: `Basic base64(email:token)` as a secret, for
  Jira and Bitbucket.

**Validation:** a Go test validates every entry:
- the fields are referenced correctly;
- every `creates` item renders, and passes `prepare` with dummy values;
- every `available` entry has a test, or explains why not.

### 2. One generic connect engine

`catalog.Connect(entry, values) → items + secrets + done-info` replaces
the hand-written `addGitHub`/`addGitLab`/`addAWS`.
- **One commit:** all of a service's items are written in **one**
  `commit` (one revision), through `putItems`, so `checkOverlay`,
  `credsCheck` and the audit all apply, as today.
- **AWS:** keeps its special credential logic as a catalogue entry with
  a small Go hook (`hooks: aws`), because STS profiles and roles don't fit
  pure templating.

**Connection label.** Every created item gets the additive field
`connection: <name>` (on sources and credentials; it's metadata only).
- **Connected** groups by this label.
- **Older items** (from before the catalogue) without a label are
  grouped by the current heuristics, and shown as before.

**API:**
- `GET /api/catalog`: the entries without templates, plus their
  availability on this install. `needs-package` is computed from
  `server.mcp_packages`.
- `POST /api/services/{id}` `{fields…}`: returns the done-info, with the
  one-time secret and `no-store`.
- `POST /api/connections/{name}/test`.
- **Compatibility:** the old `/api/services/github|gitlab|aws` routes stay
  as aliases.

**CLI:**
- `siphon catalog [--category c] [-o json]`;
- `siphon connect <id> --field value …` (secret fields only via `-` or
  `@file`);
- **Compatibility:** `connect github|gitlab|aws` keep their current flags
  as aliases of the catalogue fields.

### 3. Webhook verification

| Mode | Algorithm |
|---|---|
| `slack` (new) | `v0=` + hex HMAC-SHA256 of `"v0:" + X-Slack-Request-Timestamp + ":" + body`, vs `X-Slack-Signature`; ±300 s. |
| `stripe` (new) | parse `Stripe-Signature` into `t` and the `v1` values; hex HMAC-SHA256 of `t + "." + body`; any `v1` matches; ±300 s. |
| `sha256` (new options) | `signature_prefix:` (e.g. `v1=`), and the header may hold a comma-separated list (any match), for PagerDuty. `timestamp_separator:` (default `.`; `:` for Grafana `ts:body`). |

- All comparisons are constant-time, and the replay windows are enforced.
- **Tests** use each provider's documented example vector where one is
  published, or a vector computed from the documented algorithm (named
  as such).
- **Delivery IDs:** the `slack` and `stripe` modes use the signed
  timestamp plus the body hash as the delivery key, as `sha256` does
  today.

### 4. Local MCP packages

**Module option:** `services.siphon.catalogPackages` (a list of catalogue
IDs, default `[]`) adds the Nix-pinned MCP servers to `mcpPackages`, at
option-default priority (as `aws.enable` does). It covers:
- `grafana` → `mcp-grafana`, with `--disable-write`;
- `gitea` → `gitea-mcp-server`;
- `forgejo` → `forgejo-mcp`;
- `kubernetes` → `mcp-k8s-go`.

The env names and hosts come from `nix/catalog-packages.json`, which a
`go generate` step produces from the catalogue. A docs-style test fails
if it's stale, so Go and Nix can't drift.

**Not installed:** the entry shows `needs-package` ("enable
`services.siphon.catalogPackages = [ "grafana" ]`"). Its webhooks are
still connectable, and only tools are unavailable.

### 5. The Services page (following the Codex review)

| Section | Content |
|---|---|
| **Connected** (top) | One row per connection: monogram, name (`Linear · work`), capability chips, status pill (**Not checked** / **Working** / **Needs attention**), last event (from its sources' `source_state`), **Test** (where the entry has a test) and **Edit**. Rows stack on phones, with 44 px targets. |
| **Explore services** | Search (`GET ?q=`), category filter links (`?cat=`), and a grid of tiles (min 240 px; 36 px monogram; name, one-line summary, capability chips, an availability pill, **Connect** at the bottom). Unavailable tiles show the reason and no Connect button. There's an empty state for no results. htmx swaps the grid without a reload, and plain links work without JavaScript. |
| **`/services/{id}`** (max 640 px) | Numbered sections: **1. What to connect** (name, mode/choices), **2. Access** (token fields, with the permission help next to them), **3. Events** (a webhook toggle as one aligned row). Errors appear inline, next to the field, and keep the values typed; the primary button is at the end, with Back beside it. |
| **Done page** | As today: the one-time secret, the provider setup steps (from `setup`), and the tools to allow. |

**Status:** `Working` or `Needs attention` comes from the last test,
stored in a new `connection_check(connection, at, ok, detail)` table
(migration `0004`). Nothing is inferred from a saved config.

**Styling:** existing tokens (`--surface`, `--line` …), with focus,
hover, selected and disabled states shown by border and outline, in
light and dark. CSP-clean; any JavaScript goes in `static/app.js`.

### 6. The first wave

| Category | Entry IDs (status) |
|---|---|
| code | `github`, `gitlab` (webhooks + polling; tools `not-yet`: OAuth), `bitbucket` (Rovo MCP with a scoped token, Basic), `gitea`, `forgejo` (tools: needs-package) |
| issues | `linear`, `jira` (Rovo MCP with a Basic header, `sha256` webhooks) |
| chat | `slack` (webhooks only, `slack` mode; tools `not-yet`: OAuth) |
| monitoring | `sentry` (webhooks; tools need packaging), `pagerduty` (remote MCP with the `Token token=` header, `sha256` + `v1=` list), `grafana` (needs-package tools + webhooks), `alertmanager`, `datadog`, `uptime-kuma` (token webhooks) |
| cloud | `aws` (existing), `cloudflare` (remote MCP with Bearer + `cf-webhook-auth` token), `kubernetes` (needs-package tools) |
| payments | `stripe` (remote MCP with an **Agent key** + `stripe` webhooks) |
| home | `home-assistant` (`/api/mcp` with a long-lived token + token webhooks), `ntfy` (polling) |
| generic | `webhook` (pick a mode), `mcp` (URL + any header) |
| not yet (listed with reasons) | Discord, Microsoft Teams, Mattermost, Matrix, Notion, Google Pub/Sub, Azure Event Grid, Azure DevOps, Opsgenie (end of support) |

**Templates:** each new available service gets at least one template:
- a Linear issue from an alert;
- a Jira ticket triage;
- a Slack message → agent;
- a Sentry issue → agent;
- a PagerDuty incident summary;
- a Grafana alert explanation;
- a Stripe dispute notice;
- a Home Assistant check.

**Docs:**
- `docs/connections/README.md` is **generated** from the catalogue (a
  table per category, with status and reasons).
- The existing GitHub, GitLab and AWS pages stay. New services get their
  setup text from the entry (`setup`), so per-service pages are only
  written where there's more to say.

## Alternatives rejected

- **Keep hand-written presets:** 20 more of them means about 20 more Go
  functions and forms. Data entries with a template engine are smaller
  and testable.
- **A JSON catalogue for Nix and Go:** awkward to author (no comments).
  YAML is the source, and a generated JSON subset feeds Nix, with a
  freshness test.
- **A side-panel connect flow:** worse for keyboards, errors and the
  back button; the dedicated page was chosen (owner question 3).
- **Grouping by URL heuristics:** fragile; the `connection:` label was
  chosen (owner question 4).
- **OAuth now:** a separate task. OAuth-only tools are listed as
  `not-yet`.
- **Packaging Sentry's and the containers Kubernetes local servers now:**
  deferred (owner question 2).

## Risks

- **Provider changes:** webhook headers, MCP URLs and Stripe's key rules
  (2026-10-31) drift. Each entry links its docs, and `status`/`reason`
  make it cheap to mark one `not-yet`.
- **Template rendering of secrets:** values go only through `secret` and
  `basic`, never inline. A test renders every entry with sentinel secret
  values and asserts that no sentinel appears in any YAML.
- **Unverified research items** (the PagerDuty header scheme, the
  Datadog MCP URL, Matrix): such entries start as `not-yet` until
  verified by a live test, or by the owner.
- **The page redesign changes the portal's look:** the existing web
  tests are kept (adapted). Screenshots in light and dark are checked
  before the PR.
- **Hosts:** none changed. The module option is opt-in.

## Verification

- **Unit tests:**
  - every catalogue entry renders and validates (dummy values);
  - no secret appears in the rendered YAML;
  - the `slack`, `stripe` and `sha256`-option vectors, plus the window
    and constant-time paths;
  - the connect engine (one revision, labels, the done-info, the secret
    shown once);
  - grouping (labelled and legacy);
  - the `connection_check` status;
  - the `/api/catalog` availability;
  - the CLI `catalog` and `connect <id>` (including the old flags);
  - `catalog-packages.json` freshness.
- **Web tests:** the Connected rows; search and filter (with and without
  htmx); the connect page's inline errors with values kept; the CSP.
- **Nix:** an eval check that `catalogPackages = [ "grafana" ]` adds the
  package. The VM subtest connects `linear` against a stub
  (`api.linear.app` → a local fake via `/etc/hosts`, plus a stub MCP),
  and `webhook` with the `slack` mode, verified by a signed delivery.
- **Docs:** the generated connections index is fresh; links resolve;
  the templates validate.
- **Owner live:** connect one real new service (for example Linear or
  Home Assistant) and see it grouped under Connected with **Working**
  after Test.
