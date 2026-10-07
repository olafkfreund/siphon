---
status: draft
issue: 23
author: olafkfreund
---

# Intent: Service integrations: GitHub, GitLab and AWS

## Problem

Siphon is built to react to the systems a team already runs. Connecting the
three the owner cares about first is either awkward or impossible today:

- **GitHub** works, but only by hand-writing YAML:
  - an MCP source with `auth.bearer`, a webhook with `signature: github`,
    and the right tool allowlist for each agent;
  - working out which token scopes to use, and which tools are safe to give
    an agent.
- **GitLab** works only half way:
  - polling through REST or MCP is possible;
  - **its webhooks can't be received**. GitLab authenticates them with a
    plain shared token in `X-Gitlab-Token`, or, with a "signing token", a
    Standard Webhooks signature. Siphon accepts only GitHub-style or hex
    HMAC signatures.
- **AWS** is mostly out of reach:
  - AWS's MCP servers are mostly local (stdio) programs that need
    credentials (`AWS_PROFILE`, a role, keys). **Siphon has no way to give a
    stdio MCP server a secret.** The same gap blocks the local GitHub MCP
    server and many others.
  - EventBridge API destinations authenticate with an API-key header, which
    Siphon can't check.
  - There is no notion of short-lived AWS credentials.
- **No guided setup.** The Connections page makes model endpoints easy, but
  nothing does the same for services: where the token goes, what scope it
  needs, and what the webhook URL is.

## Proposed outcome

1. **More webhook authentication modes** for `type: webhook` sources, each
   compared in constant time and each documented with the exact provider
   setting:
   - **`token`:** a shared secret in a named header, for example GitLab
     `X-Gitlab-Token` or an EventBridge API-key header.
   - **`standard-webhooks`:** the Standard Webhooks HMAC signature
     (`webhook-id`, `webhook-timestamp`, `webhook-signature`, with a replay
     window). GitLab signing tokens use it, and so do other modern
     providers.

   The existing `github` and `sha256` presets stay.
2. **Secrets for local (stdio) MCP servers.**
   - A source gets an `env:` map whose values are secret references
     (`file:`/`env:` and portal-stored secrets, under the same rules as
     today).
   - The values are injected **only into that MCP server's process**, both
     when an agent uses it inside the sandbox and when Siphon polls it.
     They are never put in argv, never shown, and always masked.
3. **AWS done safely.**
   - Siphon supports running an AWS MCP server with **short-lived**
     credentials: a role assumed for each run, or a `credential_process` or
     profile, instead of long-lived keys.
   - The docs recommend a **read-only IAM role** scoped to what the agent
     needs.
   - The egress allowlist for such an agent covers only the AWS endpoints
     it needs.
4. **A Services page with presets for GitHub, GitLab and AWS.** Each tile
   guides the user, like the model tiles:
   - **GitHub:** a fine-grained token (with the scopes to pick), the remote
     MCP server, an optional webhook (showing the URL to paste into GitHub
     and generating the secret), and recommended **read-only** and
     **read-write** tool allowlists to choose from.
   - **GitLab:** a token, the MCP or REST source, and a webhook with
     `token` auth (generated, and shown once to paste into GitLab).
   - **AWS:** a region, a role, or a profile, plus the AWS MCP server, with
     a read-only policy template.
   - Saving creates ordinary, editable items (sources, webhook,
     credentials), so nothing is hidden or magic. A **Test** button checks
     each one: the token works, the scopes are as expected, and the MCP
     server lists tools.
5. **Tested and documented.**
   - Unit and VM tests for each new webhook mode and for stdio MCP secrets
     (the secret reaches only that process).
   - One owner-run check per service: a GitHub PR webhook triggers an agent
     that reads the PR; a GitLab merge request event is accepted; an AWS
     read-only call works through an MCP server under a role.
   - README sections per service, with token scopes and security notes.

## Affected users and systems

- **Operators** connecting Siphon to GitHub, GitLab or AWS. That's the main
  use case for event-driven agents.
- **The repo:**
  - `internal/source` (webhook auth modes, stdio MCP env);
  - `internal/config` (new fields, presets);
  - `internal/action` and `internal/agentloop` (passing env to stdio MCP
    servers in the sandbox);
  - `internal/web` (the Services page and tests);
  - the NixOS module (if AWS role or profile support needs host config);
  - the VM test and the README.
- **External accounts,** for the owner's checks only: a GitHub repo, a
  GitLab project, and an AWS account with a read-only role. These need the
  owner's tokens and setup. Nothing is created in them without the owner's
  go-ahead.

## Constraints

- **All the guarantees of #12/#15/#20 hold:**
  - a portal or API token holder can't widen host privileges or read host
    secrets;
  - secrets are references, write-only and masked;
  - agents get only allowed tools and allowed hosts;
  - stdio `command` stays file-only (F1).
- **A stdio MCP server's secrets must not reach the agent's own process,
  other MCP servers, the job output or logs.**
- **Webhook modes** are verified in constant time before any processing.
  The replay protection that exists today applies to every mode.
- **AWS:**
  - no long-lived keys stored by default;
  - credentials are scoped per run where possible;
  - nothing calls AWS APIs that change state unless an operator allowlists
    those tools explicitly.
- **No vendor SDKs** unless they're unavoidable. AWS short-lived credentials
  may need STS, which can be done with a small signed request or by using
  the AWS CLI's `credential_process` (open question 3).

## Open questions

1. **Stdio MCP `command` stays file-only (F1)**, so the Services page can't
   add a *local* MCP server from the portal. Options:
   - (a) presets that need a local server (the AWS MCP server, the local
     GitHub server) are file or NixOS only, and the page shows the snippet
     to add;
   - (b) ship a **fixed allowlist of known MCP server packages** that the
     portal may enable (for example `awslabs.aws-api-mcp-server` and
     `github-mcp-server`, pinned through Nix);
   - (c) both.

   Proposal: (c). The portal can enable only allowlisted, Nix-pinned servers
   and never pick an arbitrary binary. Anything else is file-only.
2. **GitHub:** prefer the remote MCP server (`api.githubcopilot.com`,
   simple, token only) or the local `github-mcp-server` (more control, works
   with GitHub Enterprise)? Proposal: remote by default, local as an
   option.
3. **AWS credentials:** (a) `sts:AssumeRole` per run using a base identity
   from the host (an instance role, or a profile in a credentials file);
   (b) the operator's `credential_process` or SSO profile, used as is; (c)
   long-lived keys, as a discouraged fallback only. Proposal: (a) and (b),
   with (c) off by default.
4. **GitLab:** support gitlab.com and self-hosted GitLab (base URL setting)
   from the start? Proposal: yes. It costs only a URL field.
5. **Scope:** all three services in this issue, or GitHub and the generic
   building blocks first (webhook modes, stdio env, presets), then GitLab,
   then AWS as follow-ups? Proposal: the building blocks plus GitHub and
   GitLab here, and AWS as its own issue, since it has the most security
   surface.
