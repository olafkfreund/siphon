---
status: approved
issue: 59
author: olafkfreund
---

# Intent: templates for the catalog services that have none

## Problem

All the services in `internal/catalog/services.yaml` are `available`, but
five have no ready-made task in `docs/templates/`, so `siphon template` and
the portal offer nothing for them once they are connected:

| Service | What the catalog connects |
|---|---|
| Bitbucket | Atlassian Rovo MCP tools, repository webhooks |
| Gitea | local `gitea-mcp-server` tools, webhooks |
| Forgejo | local `forgejo-mcp` tools, webhooks |
| Datadog | monitor webhooks only (no MCP yet) |
| Cloudflare | Cloudflare remote MCP tools, notification webhooks |

## Proposed outcome

One template per service, each a real task in the style of the existing
ones, listed in `docs/templates/README.md`:

- `bitbucket-pr-review`: review new pull requests with an agent;
- `gitea-pr-review` and `forgejo-pr-review`: the same on the self-hosted forges;
- `datadog-monitor-alert`: summarise a triggered monitor with a local
  model, then notify;
- `cloudflare-notification`: notify on Cloudflare notifications, with an
  agent that can look things up with Cloudflare's tools (read-only).

Each one passes `siphon apply --dry-run` and the existing template tests.

## Affected users and systems

- `docs/templates/*.yaml` and `docs/templates/README.md` (embedded in the binary).
- Nothing in the pipeline or the catalog.

## Constraints

- Templates must match the catalog's real event shapes and header names,
  taken from each vendor's webhook docs.
- Agent actions are read-only by default and need approval, like the
  existing review templates. `cooldown:` on every agent action.
- No secrets in templates, only `--secret` hints in the header.

## Open questions

- Is one template per service enough, or do you want two for the forges
  (PR review and CI failure)?
- Gitea and Forgejo send near-identical payloads. Two templates, or one
  that covers both? I'd write two, so each is found by name.

## Decisions at approval

- One template per service; Gitea and Forgejo get one each, so each is found by name.
