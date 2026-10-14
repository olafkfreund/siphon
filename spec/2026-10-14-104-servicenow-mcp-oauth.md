---
status: draft
issue: 104
intent: intent/2026-10-14-104-servicenow-mcp-oauth.md
---

# Spec: Siphon's OAuth login works with ServiceNow's official MCP server

## What ServiceNow serves (fetched live on 2026-10-14)

**MCP endpoint:** `https://<inst>/sncapps/mcp-server/mcp/sn_mcp_server_default`.
Call it X below.

| Request | Answer |
|---|---|
| POST X without a usable token | `302` to the browser login |
| `/.well-known/oauth-protected-resource<X path>` | `resource: X`, `authorization_servers: [X]` |
| `/.well-known/oauth-authorization-server<X path>` | `issuer: https://<inst>` (bare), endpoints `/oauth_auth.do` and `/oauth_token.do`, PKCE S256, no `registration_endpoint` |
| `/.well-known/oauth-authorization-server` (bare) | `404` |
| `/.well-known/openid-configuration` (bare) | `302` |

**Two places where go-sdk v1.8.0 fails:**
1. **No challenge.** `StreamableClientTransport` calls `OAuthHandler.Authorize`
   only on a 401 or 403. A 302 surfaces as `sending initialize: Found`.
2. **Issuer mismatch.** go-sdk takes the authorization server's issuer from
   the protected-resource metadata, which is X. It then fetches X's
   path-suffixed RFC 8414 document and requires `issuer == X`
   (`oauthex.GetAuthServerMeta`). The document says the bare instance, so
   discovery fails. The bare instance's own metadata URLs return 404 and 302,
   so naming the bare instance alone doesn't help either.

## Design

Two small `http.RoundTripper`s. Both apply only to sources with
`auth.oauth`.

### 1. Turn a redirect into a challenge

**Where:** in `internal/source`, next to `bearerTransport`.

```go
// redirectChallenge turns a 3xx from the MCP endpoint into the 401 the MCP
// auth spec requires; some servers (ServiceNow) redirect to a browser login.
type redirectChallenge struct{ base http.RoundTripper; endpoint string }
```

**When it fires:** the request URL equals the source's `url`, and the
response status is 300–399.

**What it returns:**
- the body is drained and closed;
- `401 Unauthorized` with
  `WWW-Authenticate: Bearer resource_metadata="<RFC 9728 path-suffixed URL for the endpoint>"`;
- an empty body.

Every other response passes through untouched. The guarded clients already
refuse to follow redirects (`netguard.go:54,59`), so the 3xx is always
visible here.

An exported `source.RedirectChallenge(base, endpoint) http.RoundTripper`
wraps it for two callers:

- **Steady state.** `connectTransport` (`internal/source/mcp.go:196-200`)
  wraps the client when `o.OAuth != nil`. On a later 302, the existing
  `handler.Authorize` (`mcpoauth.go:285`) returns `ErrLoginRequired`.
  - The source then shows "login required", which is intent decision 2.
  - It never shows "Found".
- **Login.** `Manager.run` (`mcpoauth.go:507`) gives the
  `StreamableClientTransport` an `http.Client` that copies `hc` with the
  wrapped transport. Only the MCP calls go through the wrapper; discovery
  and token calls keep `hc`.

### 2. Accept a pinned issuer that serves its metadata under a path

**Where:** in `internal/mcpoauth`, used only by the login's discovery
client.

**When it is active:** only when `auth.oauth.issuer` is set. Call that pin P.

**The rule:** the protected-resource metadata names one authorization
server X. X must be on the same origin as P, and X's path must start with
P's path, with X ≠ P. When that holds, the round tripper does two things:

- **It rewrites the protected-resource metadata response**, changing
  `authorization_servers` to `[P]`.
- **It sends requests for P's RFC 8414 URL** (`<P origin>/.well-known/oauth-authorization-server<P path>`)
  to X's path-suffixed URL instead. It returns that document only if its
  `issuer` equals P. Otherwise it returns the original response.

**The result:** everything downstream sees P:
- `asm.Issuer`;
- the pre-registered client's `Issuer` check (`authorization_code.go:558`);
- the RFC 9207 `iss` comparison;
- the stored login's `Issuer`.

**Why it's safe:**
- Nothing is trusted that the operator didn't pin.
- X and P share an origin, so no other host is ever contacted.
- When P isn't set, the round tripper isn't installed.
- When the condition doesn't hold, every response passes through, and
  go-sdk's own checks fail as they do today.

### Scope of change

- **`internal/source/mcp.go`:** `redirectChallenge`, its constructor, and
  the one-line wrap in `connectTransport`.
- **`internal/mcpoauth/mcpoauth.go`:**
  - the issuer alias round tripper;
  - in `run()`, the discovery client (`hc` with the alias when P is set) and
    the MCP client (`hc` with `RedirectChallenge`).
- **Tests** (below).
- **Docs:** `docs/connections/servicenow.md` gets a section on the official
  MCP server, with the source YAML (`url`, `auth.oauth.client_id`,
  `client_secret`, `issuer: https://<inst>`) and the OAuth app's redirect
  URL. Then regenerate `llms-full.txt`.
- **Catalog, if the live check passes (#76 step 4):** the `servicenow`
  service gets an optional `mcp_mode` field, `builtin` (the default) or
  `official`.
  - `official` creates the mcp source with `url: <instance>/sncapps/mcp-server/mcp/<server>`
    (a field, defaulting to `sn_mcp_server_default`), plus the
    `auth.oauth` fields above, instead of `package: servicenow`.
  - If the live check fails, this part is dropped and the findings go into
    the docs and onto #104.

## Alternatives rejected

- **Follow the redirect.** It lands on an HTML login page, and the guarded
  clients deliberately refuse redirects.
- **A config flag such as `auth.oauth.redirect_is_challenge`.** It's not
  needed: a redirect is never a valid answer to an MCP POST, so treating it
  as a challenge breaks no correct server. The intent's constraint is met by
  limiting it to `auth.oauth` sources.
- **Rewrite the metadata's issuer to X (the reverse alias).** Then the pin
  P no longer matches `asm.Issuer`. Dropping the pin check to compensate
  would also weaken it for ordinary servers.
- **Point `authorization_servers` at P alone.** P's own well-known URLs
  return 404 and 302, so go-sdk falls back to the guessed `P/authorize`
  endpoints, which are wrong.
- **Patch or fork go-sdk.** A fork to maintain, against "no new
  dependency". An upstream issue can still be filed for the issuer shape.
- **Hard-code ServiceNow paths.** The rule above is generic: same origin,
  a path prefix, and the operator's pin.

## Risks

- **ServiceNow may not accept the token** (the intent's unknown), so a
  valid token still gets a 302. The steady state then reports "login
  required" after a successful login.
  - The live check exposes this. The docs then say so, and the catalog
    option isn't added.
  - No loop: `handler.Authorize` never starts a login by itself.
- **A server that legitimately redirects the MCP endpoint** to another URL
  (http→https, a moved path) now shows "login required" for `auth.oauth`
  sources instead of "Found". Both are failures today. The new message is
  less precise, so the error text adds "(the server redirected)".
- **The alias rewrites a metadata body.** A parse error or an unexpected
  shape passes the original response through, and go-sdk fails as it does
  today.
- **Hosts:** none. This is a client-only change, with no migration and no
  config schema change. The catalog field is the only schema-visible
  addition, and only if it ships.

## Verification

- **Unit, `internal/source`:**
  - A 302 from the endpoint becomes a 401 with the `resource_metadata`
    challenge.
  - A 302 from another URL, and a 200 from the endpoint, pass through.
  - Through `connectTransport`, an OAuth source answering 302 gives an
    error wrapping `ErrLoginRequired`, and the text doesn't contain "Found".
- **Unit, `internal/mcpoauth`:** an `httptest` fake that reproduces the
  table above exactly: 302 without a token, path-suffixed PRM and AS
  metadata, the bare issuer, no registration, `/oauth_auth.do` and
  `/oauth_token.do`, and an MCP endpoint that accepts the issued token.
  - With `issuer` pinned and a pre-registered client, the login completes
    and `tools/list` works. The stored login's issuer is P.
  - Without the pin, the login fails with the issuer mismatch, as today.
  - With a pin on a different origin, or one not a prefix of X, it fails.
  - A token is never in an error string.
- **The suite:** `go vet ./... && go test -race ./...`, and the schema test
  if the catalog field ships.
- **Live, on `calitiiltddemo3`:**
  1. In siphon-dev, set the official mcp source with the "Siphon MCP" app.
  2. Log in, with the consent scripted under the admin login per intent
     decision 1.
  3. Run `siphon test`, or the portal's Test, which runs `tools/list`.
  4. Record the result on #104.
