# Service catalogue research (2026-10-08)

A research agent checked 28 candidates against official docs (URLs were
cited in its report). In this file, "nixpkgs" means `nix eval
nixpkgs#<attr>` succeeded on unstable. "Unverified" marks a claim that
couldn't be confirmed in the official docs.

Siphon's webhook signature modes today:
- `github`: HMAC-SHA256 hex in `X-Hub-Signature-256`, with a `sha256=`
  prefix;
- `sha256`: HMAC-SHA256 hex of the body in a named header, with an
  optional `sha256=` prefix and an optional signed timestamp (`ts.body`);
- `standard-webhooks`;
- `token`: a shared secret in a header.

## Per service

| Service | Webhook → Siphon mode | MCP for agent tools | Token to recommend |
|---|---|---|---|
| GitHub | `github` | remote `api.githubcopilot.com/mcp/` (PAT as Bearer, `/readonly` path); local `github-mcp-server` (nixpkgs) | fine-grained PAT, chosen repos, read |
| GitLab | `standard-webhooks` (signing token) or `token` (`X-Gitlab-Token`) | remote is **OAuth only**; no official local server | project token `read_api` (for polling) |
| Bitbucket Cloud | `sha256`, header `X-Hub-Signature`, `sha256=` prefix | Atlassian Rovo MCP; Bitbucket tools take a **scoped API token** (admin must enable) | scoped API token |
| Gitea / Forgejo | `github` (they also send `X-Hub-Signature-256`) | local `gitea-mcp-server`, `forgejo-mcp` (both nixpkgs), token in env | scoped token, `read:repository`/`read:issue` |
| Azure DevOps | basic auth only → `token` on Authorization (or a basic-auth mode) | remote Entra OAuth only; local npm `@azure-devops/mcp` takes a PAT (not in nixpkgs) | PAT: Code/Work Items read |
| Linear | `sha256`, header `Linear-Signature`, no prefix (replay via the body's `webhookTimestamp`, ±60 s) | remote `mcp.linear.app/mcp` takes **an API key as Bearer** | personal API key |
| Jira Cloud | `sha256`, `X-Hub-Signature`, `sha256=` | Rovo MCP `mcp.atlassian.com/v2/mcp`: a personal token as **Basic email:token**, or a service-account key as Bearer; admin enables it | service-account scoped key |
| Slack | **new `slack` mode** | remote is **OAuth only** | bot token `xoxb-` |
| Discord | **new `ed25519` mode**, plus a PING → 204 handshake; doesn't cover channel messages | none | bot token |
| MS Teams | **new `teams-hmac` mode** (sync reply within 5 s) | Entra OAuth, preview | n/a |
| Mattermost | **new `body-token` mode** (the token is in the body) | plugin-embedded; standalone is dev-only | bot PAT |
| Matrix | `token` (Bearer `hs_token`), but PUT and a `{}` reply are needed | none | as_token |
| Sentry | `sha256`, `Sentry-Hook-Signature`, no prefix | remote OAuth only; local npm `@sentry/mcp-server --access-token` (not in nixpkgs) | auth token |
| PagerDuty | `sha256` **extended**: `v1=` prefix and a comma list (rotation) | remote `mcp.pagerduty.com/mcp` with `Authorization: Token token=<key>` (unverified); the local server was archived 2026-09-04 | read-only API key |
| Opsgenie | `token` (custom headers) | none; **end of support 2027-04-05** | (skip) |
| Grafana | `sha256`, `X-Grafana-Alerting-Signature`; with a timestamp the input is `ts:body` (**needs a separator option**) | local `mcp-grafana` (nixpkgs), `GRAFANA_SERVICE_ACCOUNT_TOKEN`, `--disable-write` | service account, Viewer |
| Alertmanager | `token` (`http_config` authorization or headers) | via `mcp-grafana`, or `prometheus-mcp-server` (not in nixpkgs) | bearer at a proxy |
| Datadog | `token` (custom headers) | remote MCP (the URL varies by site): a PAT as Bearer, or DD_API_KEY + DD_APPLICATION_KEY headers | service-account app key |
| Uptime Kuma | `token` (additional headers) | none | — |
| Cloudflare | `token` (`cf-webhook-auth`) | remote `mcp.cloudflare.com/mcp`: an API token as Bearer | account API token, minimal |
| Kubernetes | n/a | `mcp-k8s-go` (nixpkgs); `containers/kubernetes-mcp-server` (not in nixpkgs, `--read-only`) | ServiceAccount with `view` |
| Hetzner | no webhooks: poll `GET /v1/actions` | community only | read token |
| GCP Pub/Sub push | **new `google-oidc` mode** (RS256 JWT, JWKS, aud/email/exp) | OAuth/IAM only | push service account |
| Azure Event Grid | `token` header, **plus the validation handshake** | `azure-mcp`, `aks-mcp-server` (nixpkgs; auth unverified) | Reader SP |
| Stripe | **new `stripe` mode** | remote `mcp.stripe.com`: an **Agent API key** as Bearer; **from 2026-10-31 other keys are rejected** | Agent key, least permissions |
| Notion | `sha256`, `X-Notion-Signature`, `sha256=`, **plus bootstrapping the `verification_token`** | remote OAuth only; the local server is unmaintained | internal integration token |
| Home Assistant | `token` (`rest_command` headers) | built-in `mcp_server` `/api/mcp`, with a **long-lived token**; `ha-mcp` (nixpkgs) | long-lived token, non-admin user |
| ntfy | poll `GET /<topic>/json?poll=1&since=` with Bearer | none | read-only token |
| Any webhook / any MCP | user's choice | remote MCP needs an **arbitrary auth header** (Atlassian Basic, PagerDuty `Token token=`, Datadog's two headers) | — |

## New signature modes

- **`slack`:** `hex(HMAC-SHA256(secret, "v0:" + X-Slack-Request-Timestamp + ":" + body))` vs `X-Slack-Signature` minus `v0=`, ±300 s.
- **`stripe`:** parse `t` and the `v1` values from `Stripe-Signature`; `hex(HMAC-SHA256(secret, t + "." + body))`; any `v1` may match; ±300 s.
- **`sha256` extensions:** a configurable prefix (`v1=`) with a comma-separated list (PagerDuty); a timestamp separator (`:` for Grafana, `.` today).
- **Heavier, not first wave:**
  - `ed25519` (Discord, with PING);
  - `teams-hmac`;
  - `body-token` (Mattermost);
  - `google-oidc` (Pub/Sub JWT);
  - the handshakes for Event Grid and Notion, and Matrix's PUT.

## OAuth-only MCP, with no token alternative

GitLab remote, Slack, Notion, MS Teams, Azure DevOps remote, and Google
Cloud remote. Sentry and Azure DevOps have official local servers that take
a token, but neither is in nixpkgs.
