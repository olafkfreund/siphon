---
status: draft
issue: 52
author: olafkfreund
---

# Intent: let the operator pin an MCP source's authorization server

## Problem

**Today's behaviour:**
- **Discovery:** an MCP source with `auth.oauth` signs in through
  whichever authorization server the MCP server's protected-resource
  metadata names.
- **Pinning:** with a preregistered `client_id`, that server is pinned
  only after the first login (`internal/mcpoauth/mcpoauth.go`, the
  `oc.ClientID != ""` case: "trust on first use").
- **Where the pin comes from:** the `iss` response parameter, which many
  authorization servers don't send. Then nothing is pinned at all.

**What an attacker can do:** a malicious or compromised MCP server, or
anyone who can change its metadata, can name its own authorization
server. The first login (or every login, when there's no `iss`) then
goes there:
- the configured `client_secret` is sent to it at the token step;
- the user signs in to a page the attacker controls.

go-sdk already refuses an authorization server that doesn't match a
preregistered client's `Issuer` (`auth/authorization_code.go:558`).
Siphon just never gives it one up front.

## Proposed outcome

- **A new setting:** a source's `auth.oauth` can name its authorization
  server, `issuer: https://login.example.com`.
- **With it set,** a login whose discovered authorization server differs
  fails with a clear error, before any secret or browser redirect leaves
  Siphon. That holds on the first login too.
- **Without it,** behaviour is unchanged: trust on first use.
- **Docs** say when to set it: always, when there's a `client_secret` or
  the MCP server isn't your own.

## Affected users and systems

- `internal/config` (the `OAuth` struct, `Validate`, the schema) and
  `internal/mcpoauth` (passing the pin to go-sdk).
- Operators and users who connect remote MCP servers with OAuth, through
  `siphon.yaml`, the portal or `siphon apply`.
- The docs for MCP OAuth connections, and `siphon explain`, if it lists
  `auth.oauth` fields.
- No migration. No hosts.

## Constraints

- **Must:**
  - fail closed on a mismatch, before the client secret or the user's
    browser reaches the other server;
  - keep existing configs working unchanged;
  - accept only `https` (or `http` on loopback), like `server.oidc.issuer`.
- **Must not:**
  - add a dependency;
  - log or echo secrets in the mismatch error.

## Open questions

1. **Who may set it:**
   - **(a)** like the rest of a source's `auth.oauth`: portal and API
     users too. My proposal: whoever can change the source's `url` can
     already choose which server is trusted.
   - **(b)** operator-only.
2. **Should it be required when a `client_secret` is set?** That would
   break existing configs.
   - **(a)** No, but `Validate` warns when it's missing. My proposal.
   - **(b)** Yes, required.
3. **Changing the issuer after a login:** a stored login bound to a
   different issuer is no longer reused, so the next login starts fresh.
   I propose yes.
