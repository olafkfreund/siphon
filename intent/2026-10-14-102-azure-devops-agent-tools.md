---
status: draft
issue: 102
author: olafkfreund
---

# Intent: Azure DevOps tools for agents

## Problem

#89 shipped Azure DevOps service-hook webhooks. Agents still have no Azure
DevOps tools: they can't read a failed build's log, look up a work item, or
comment on a pull request. The catalog says "Agent tools come later."

Microsoft offers two MCP servers. Neither works from Siphon today.

### 1. The remote server, `https://mcp.dev.azure.com/{org}`

Microsoft recommends it. It's hosted, streamable HTTP, and has headers that
narrow it: `X-MCP-Toolsets`, `X-MCP-Readonly` and `X-MCP-Tools`.

What I probed on 2026-10-14, all public, with no credentials:
- **It behaves to spec:** a `401` with
  `WWW-Authenticate: Bearer resource_metadata=".../.well-known/oauth-protected-resource/{org}"`.
- **Its protected-resource metadata** names Entra's multi-tenant authority,
  `https://login.microsoftonline.com/organizations/v2.0`, with the scope
  `https://mcp.dev.azure.com/.default`.
- **There's no dynamic client registration.** Microsoft's docs require a
  custom Entra app registration for a client like Siphon:
  - a delegated permission on the "Azure DevOps MCP" enterprise app;
  - a redirect URI;
  - for a web redirect, a client secret.

  Siphon's `auth.oauth.client_id` and `client_secret` fit this.
- **Two go-sdk refusals stop the login before it starts:**
  - **Issuer.** The metadata at
    `/organizations/v2.0/.well-known/openid-configuration` says
    `issuer: https://login.microsoftonline.com/{tenantid}/v2.0`. That's a
    template, so it never equals the URL it was fetched from. A
    tenant-specific URL (`/<tenant>/v2.0`) has an exact issuer, but the
    metadata doesn't name it.
  - **PKCE.** Entra supports S256 but doesn't list
    `code_challenge_methods_supported`. go-sdk refuses a server that
    doesn't, with "does not implement PKCE".

### 2. The local server, `@azure-devops/mcp`

It runs from npm with Node ≥ 20, over stdio.

- **Usage:** `@azure-devops/mcp <org> -a pat`, with `PERSONAL_ACCESS_TOKEN`
  set to base64(`<email>:<pat>`). `-d core work-items pipelines …` limits
  the tool domains.
- **It isn't in nixpkgs.** Siphon only runs pinned packages
  (`nix/catalog-packages.json`, `nix/pkgs/*.nix`), so it needs packaging.
- **A PAT is a long-lived user secret,** but it can be scoped (for example
  *Work Items: Read & write*, *Build: Read*) and set to expire.

## Proposed outcome

The `azure-devops` catalog card gains agent tools. An agent can read and
update work items, read builds and their logs, and comment on pull
requests, with the read-only, toolset or domain filters set at connect time.

- **The local path (PAT):** Siphon ships a pinned `azure-devops` MCP package
  (a Nix-built `@azure-devops/mcp`).
  - Connecting asks for the organization and a PAT. Siphon does the base64
    `email:pat` encoding, so the user never has to.
  - It runs under the bridge, with egress limited to `dev.azure.com` and its
    `*.visualstudio.com` hosts.
- **The remote path (Entra):** if it's in scope (see the open questions),
  `auth.oauth` gets the narrowest Entra shims that make go-sdk's login work:
  - the operator pins the tenant issuer;
  - PKCE S256 is assumed only for that pinned Entra issuer.

  It's documented with the app-registration steps.
- **Templates:** a failed build → an agent reads the log and comments on
  the work item or PR. Reuse the #89 webhook templates where possible.

## Affected users and systems

- Azure DevOps users of the catalog.
- `internal/catalog/services.yaml` and `nix/catalog-packages.json`, plus a
  new `nix/pkgs/azure-devops-mcp.nix` and its wiring in `flake.nix` and
  `nix/module.nix`.
- `docs/connections/` and the templates.
- **For the remote path:** `internal/mcpoauth`, the same area as #104.

## Constraints

- **No secret in a file, a flag value or the output.**
  - The PAT goes only to the bridged child's env.
  - The base64 encoding happens in the daemon. Never on the command line.
- **The package is pinned, built by Nix, and reproducible** (`npmDepsHash`,
  `nix build --rebuild`). Nothing like `npx` fetches at runtime.
- **Any Entra shim needs an explicit operator pin,** never trust on first
  use. It must not loosen OAuth for other servers. #104's pinned-issuer rule
  is the model.
- **Restricted egress must hold:** the bridge's `url_env` or `hosts` list
  only Azure DevOps hosts.

## Open questions

1. **Scope.** Do the local PAT path first and leave the remote Entra path
   as a follow-up? Or both now? **I'd do PAT now.** It needs no tenant
   admin, and it's testable with a free Azure DevOps org. The remote path
   needs Entra shims and an admin-consented app registration.
2. **A test organization.** Do you have an Azure DevOps organization (for
   example under the Calitii Entra tenant) and a PAT I can use for the live
   check? Its credentials would go in agenix, like ServiceNow's. If not, I'd
   verify against a stub in the VM test only, and you'd do the live check.
3. **The default tool set.** Should the catalog default to read-only
   domains (`core work-items pipelines repositories`, read tools only in
   the template's `allowed_tools`)? Or expose writes too and rely on
   approvals? **I'd default to read-only tools, with writes opt-in,** as
   ServiceNow does.
