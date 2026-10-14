---
status: approved
issue: 104
author: olafkfreund
---

# Intent: Siphon's OAuth login works with ServiceNow's official MCP server

## Problem

The #76 spike found that Siphon can't log in to ServiceNow's official MCP
server, the Now Assist "MCP Server Console", such as
`/sncapps/mcp-server/mcp/sn_mcp_server_default`.

- **How the login starts:** Siphon's login (`internal/mcpoauth`, `run`)
  connects as an MCP client with the go-sdk's authorization-code handler.
  The SDK starts OAuth only when the server answers **401** with a
  `WWW-Authenticate` challenge, as the MCP spec requires.
- **What ServiceNow does instead:** it answers **302** to its browser
  login (`/auth_redirect.do`). The login fails at once with
  `calling initialize: sending initialize: Found`.
- **The discovery documents are fine:**
  - the RFC 9728 protected-resource metadata is at
    `/.well-known/oauth-protected-resource/<mcp path>`;
  - the RFC 8414 authorization-server metadata is at
    `/.well-known/oauth-authorization-server/<mcp path>`, but its `issuer`
    is the bare instance URL, not the URL it was discovered under;
  - there's no dynamic client registration, so a pre-registered client is
    needed. Siphon's `auth.oauth.client_id`, `client_secret` and `issuer`
    already support that.
- **Unknown:** whether ServiceNow accepts a token Siphon obtains this way.
  A client-credentials bearer and HTTP Basic also got 302.

## Proposed outcome

- **A redirect counts as the challenge.** For an MCP source with
  `auth.oauth`, a 3xx answer from the MCP endpoint is treated as an
  authentication challenge. A redirect is never a valid answer to an MCP
  POST.
  - Siphon synthesises the `401` and
    `WWW-Authenticate: Bearer resource_metadata="<protected-resource metadata URL for this endpoint>"`
    that the SDK expects.
  - The standard discovery and authorization-code flow then run.
- **ServiceNow's issuer shape is accepted,** but only when it's safe:
  `auth.oauth.issuer` is pinned, and the authorization-server metadata's
  `issuer` equals the pin.
- **A test reproduces ServiceNow's behaviour exactly:** the 302, the
  path-suffixed metadata, the issuer mismatch and no registration endpoint.
  It proves the login completes against it, with no real instance.
- **A live check against `calitiiltddemo3`:** the login with the "Siphon
  MCP" app, then `tools/list`.
  - If ServiceNow then accepts the token, the catalog gains the
    official-server choice (the skipped #76 step 4).
  - If it doesn't, the findings are documented, and the client fix still
    stands on its own.

## Affected users and systems

- Users of ServiceNow's official MCP server, and any other MCP server that
  redirects instead of challenging.
- `internal/mcpoauth`, and the source HTTP transport for MCP sources with
  `auth.oauth`. Possibly `internal/catalog` and the ServiceNow docs.

## Constraints

- **Narrow.** It applies only to MCP sources with `auth.oauth`. Others keep
  today's behaviour exactly.
- **The issuer relaxation needs an explicit pin,** never trust on first
  use. The PKCE and state checks stay as they are.
- **No new dependency.** No secret in an error or a log.

## Open questions

- **The live login needs a browser consent on ServiceNow.** You asked me to
  do these myself. I'd script it with the admin login: a ServiceNow login
  session, the consent form, then the callback. If the consent page can't
  be scripted reliably, I'll ask you for one click. Is that OK?
- **The same redirect handling during normal use.** Should a 302 *after* a
  successful login (an expired session) also be treated as "log in again"?
  **I'd treat it the same way:** the source shows "login required", rather
  than an opaque "Found" error.

## Decisions (at approval)

- **Live consent:** scripted with the admin login. If the page resists
  scripting, I ask the owner for one click.
- **A 302 after a login** counts as "log in again". The source shows "login
  required", not "Found".
