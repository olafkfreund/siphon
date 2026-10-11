---
status: approved
issue: 52
intent: intent/2026-10-10-52-oauth-issuer.md
---

# Spec: pin an MCP source's authorization server (`auth.oauth.issuer`)

## Design

### Config (`internal/config`)

```yaml
sources:
  linear:
    type: mcp
    url: https://mcp.linear.app/mcp
    auth:
      oauth:
        issuer: https://linear.app        # new, optional
        client_id: abc123
        client_secret: file:/run/secrets/linear-client-secret
```

- **The field:** `OAuth.Issuer string` (`yaml:"issuer"`), in `config.go:364`.
- **`validateOAuth`** (`:957`): when it's set, it must be `https`, or
  `http` on a loopback host. That's the same rule as `server.oidc.issuer`
  (`:199-202`); both use a new helper, `secureURL(string) bool`.
  - The error: `<p>.auth.oauth.issuer: must be https, or http on a loopback host`.
- **`Warnings()`** (`:834`), in the per-source loop: a source with
  `auth.oauth.client_secret` and no `issuer` gets
  `source <name>: auth.oauth.client_secret without auth.oauth.issuer: the first login trusts whichever authorization server the MCP server names`.
  (Resolved question 2: a warning, not an error.)
- **Who can set it:** like the rest of `auth.oauth`, through `siphon.yaml`
  and through API/`siphon apply` edits to the source. It isn't secret, so
  it isn't added to the secret map in `overlay.go:369`. (Resolved
  question 1.)
- **`siphon explain`** (`explain.go:55-58`) gains `auth.oauth.issuer`:
  "oauth: pin the authorization server; a login through any other fails.
  Set it with a client_secret or an MCP server you don't run".
- **Schema:** regenerated.

### Login (`internal/mcpoauth/mcpoauth.go`, `run`, `:381-470`)

go-sdk compares a preregistered client's `Issuer` with the discovered
authorization server's metadata before it registers, redirects or sends a
secret (`auth/authorization_code.go:558`). The metadata's own `issuer` is
checked against the URL it was fetched from (`oauthex.GetAuthServerMeta`),
so a server can't claim someone else's issuer. Siphon passes the pin in
three cases:

- **Preregistered `client_id`:** `pc.Issuer = oc.Issuer` when it's set.
  Otherwise it's today's trust on first use (`prev.Issuer`).
- **A reused dynamic client (`reuse && prev.Issuer != ""`):** unchanged.
  `load` guarantees `prev.Issuer` equals the pin when one is set (see
  below).
- **New dynamic registration with a pin.** go-sdk doesn't check an issuer
  on its dynamic-registration path, so Siphon registers itself, against
  the pinned server:
  1. `auth.GetAuthServerMetadata(ctx, oc.Issuer, hc)`. If there's no
     metadata or no `registration_endpoint`, fail with
     `authorization server <issuer> does not support dynamic registration: set auth.oauth.client_id`.
  2. `oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, <today's metadata>, hc)`.
  3. Pass the result as `PreregisteredClient{ClientID, Issuer: oc.Issuer, ClientSecretAuth}`,
     so go-sdk then refuses a different discovered server.
  - The stored login stays `Dynamic: true`, keeping its secret, as now.
    The only code that reads `oc.ClientID == ""` for `Dynamic` is
    unchanged.
  - **Without a pin,** dynamic registration is unchanged (go-sdk does it).
- **What's stored:** `stored.Issuer` is `oc.Issuer` when a pin is set,
  since go-sdk has verified it. Otherwise it's `l.iss`, as today. So
  `Issuer` is set even when the server doesn't send `iss`.

### Reuse (`load`, `:137-149`)

- **When `oc.Issuer` is set** and `strings.TrimSuffix(st.Issuer, "/") != strings.TrimSuffix(oc.Issuer, "/")`,
  `load` returns `ok=false`. (Resolved question 3.)
- **The effect:** a login made before the pin, or for another issuer,
  isn't used. That covers token refresh (`:206`, `:300`), the status
  check (`:276`) and re-login (`:450`).
- **So the source shows as logged out** and needs a fresh login. A stored
  `TokenURL` from an unpinned login never receives the client secret
  again.
- **This is the same rule as changing `url` or `client_id` today.**

### Errors

- **A mismatch** surfaces through the existing `cleanErr` path. go-sdk's
  message names both issuers, which are URLs, not secrets.
- **Nothing new is logged.**

### Docs (`docs/tasks/connect-oauth-mcp.md`)

- **In "2. Add the source":** add `issuer:` to the preregistered example.
  Add a bullet, "Pin the authorization server", saying:
  - when to set it (a `client_secret`, or an MCP server you don't run);
  - how to find the value: the `issuer` in the authorization server's
    `/.well-known/oauth-authorization-server`.
- **In "When the login stops working"** (`:90`): add `issuer` to "changing
  `url` or `client_id` needs a new login".
- **Also:** `llms-full.txt` regenerated.

## Alternatives rejected

- **Make `issuer` require `client_id`, and leave dynamic registration
  unpinned.** That's simpler, but the user's browser still goes to
  whatever server a malicious MCP server names, which is the phishing
  half of the problem.
- **Pre-fetch the protected-resource metadata and compare it before
  starting go-sdk.** That's a time-of-check/time-of-use gap: the MCP
  server can answer differently on go-sdk's own fetch.
- **Inspect metadata responses in an `http.RoundTripper`.** The
  protected-resource metadata URL can come from `WWW-Authenticate` with
  any path, so it can't be recognised reliably.
- **Make it operator-only.** Rejected at the intent.

## Risks

- **An existing login stops being used** when an operator adds `issuer`,
  even if it matches, if that login was stored without an `iss`. That's
  a one-time re-login. The docs say so.
- **A wrong `issuer`:** every login fails with a mismatch error naming
  both issuers. That fails closed and is easy to read.
- **Fallback servers without metadata** (2025-03-26 era): with a pin and
  no client_id, Siphon refuses (no metadata, so no registration). With a
  client_id, go-sdk's fallback sets the issuer to the advertised URL,
  which must still equal the pin.
- **Hosts:** none.

## Verification

- **`internal/config` tests:**
  - `issuer` with `ftp://`, `http://example.com` and `not a url` fails;
    `https://…` and `http://127.0.0.1:…` pass;
  - the warning appears with a `client_secret` and no `issuer`, and not
    with both;
  - the schema is regenerated.
- **`internal/mcpoauth` tests,** with httptest MCP and authorization
  servers like the existing ones:
  - **Preregistered, pinned, mismatch:** the MCP server names server B,
    the pin is A. The login fails, and B's token endpoint is never
    called.
  - **Preregistered, pinned, match:** the login succeeds, and
    `stored.Issuer` equals the pin even when the callback had no `iss`.
  - **Dynamic, pinned:** registration goes to A's `registration_endpoint`.
    With the MCP server naming B, the login fails, and B's registration
    endpoint is never called.
  - **Dynamic, pinned, match:** succeeds.
  - **`load`:** a stored login with another `Issuer`, or with none, is
    not reused when a pin is set. It still is without a pin.
- **Builds:** `go vet ./... && go test -race ./...`, then CI's
  `nix flake check`.
