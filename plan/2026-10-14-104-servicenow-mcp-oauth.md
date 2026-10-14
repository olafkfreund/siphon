---
status: approved
issue: 104
spec: spec/2026-10-14-104-servicenow-mcp-oauth.md
---

# Plan: Siphon's OAuth login works with ServiceNow's official MCP server

## Approved decisions, carried over

**Two go-sdk v1.8.0 failures** against ServiceNow's official MCP server.
The endpoint is X = `https://<inst>/sncapps/mcp-server/mcp/sn_mcp_server_default`.

1. **No challenge.** X answers `302` (a browser login) instead of a 401. The
   SDK only calls `OAuthHandler.Authorize` on a 401 or 403, so it fails with
   "Found".
2. **Issuer shape.**
   - The protected-resource metadata, at
     `/.well-known/oauth-protected-resource<X path>`, says
     `authorization_servers: [X]`.
   - X's RFC 8414 document, at
     `/.well-known/oauth-authorization-server<X path>`, has `issuer` = the
     bare instance.
   - go-sdk requires `issuer == X` (`oauthex.GetAuthServerMeta`).
   - The bare instance's own metadata URLs return 404 and 302.
   - There's no `registration_endpoint`, so the client must be
     pre-registered.

**The fixes.** Two `http.RoundTripper`s, used only for sources with
`auth.oauth`. Nothing else changes behaviour.

- **A. The redirect becomes a challenge** (in `internal/source`).
  - **It fires** when the request URL equals the source `url` and the
    response is 3xx.
  - **It returns** the body drained and closed, plus a synthetic
    `401 Unauthorized` with
    `WWW-Authenticate: Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource<endpoint path>"`
    and an empty body.
  - **Everything else passes through untouched.**
  - **The steady state:** `handler.Authorize` returns `ErrLoginRequired`.
    The text adds "(the server redirected)" when the synthetic response is
    marked, using the header `Siphon-Redirected: 1` on the synthetic
    response only.
  - **Where it applies:** both the steady state (`connectTransport`) and
    the login's MCP calls.
- **B. The pinned issuer alias** (in `internal/mcpoauth`, login discovery
  only).
  - **When it's installed:** only when `auth.oauth.issuer` (P) is set.
  - **The condition:** the PRM names exactly one AS, X. X has the same
    scheme and host as P, X's path starts with P's path (trailing `/`
    trimmed), and X ≠ P.
  - **When the condition holds:**
    - the PRM response body is rewritten to `authorization_servers: [P]`;
    - a request for P's first RFC 8414 URL, from
      `authorizationServerMetadataURLs(P)` (for a bare P, that's
      `<origin>/.well-known/oauth-authorization-server`), is fetched from
      `<origin>/.well-known/oauth-authorization-server<X path>` instead;
    - that response is returned only if its `issuer` equals P. Otherwise the
      original request goes out as usual.
  - **When it doesn't hold, or the body won't parse:** everything passes
    through, and go-sdk fails as it does today.
- **Rejected:**
  - following the redirect;
  - a config flag;
  - rewriting the issuer to X (it breaks the pin check);
  - an AS of P only (its URLs return 404 and 302, so the fallback endpoints
    are wrong);
  - forking go-sdk;
  - ServiceNow-specific paths.
- **Owner decisions:**
  - the live consent is scripted with the admin login, or the owner gives
    one click if it resists;
  - a 302 after a login shows "login required".
- **Conditional (#76 step 4):** only if the live check proves the token is
  accepted, the catalog `servicenow` service gets the official-server
  option. Otherwise the findings go into the docs and onto #104.

## Steps

1. **`internal/source/mcp.go`, next to `bearerTransport` (:223-231).**
   - Add `type redirectChallenge struct{ base http.RoundTripper; endpoint string }`,
     its `RoundTrip`, and an exported
     `func RedirectChallenge(base http.RoundTripper, endpoint string) http.RoundTripper`.
   - `resource_metadata` is built from the endpoint's URL: `u.Path = "/.well-known/oauth-protected-resource" + u.Path`,
     and the query is dropped.
   - In `connectTransport` (:196-200), when `o.OAuth != nil`, set
     `client.Transport = RedirectChallenge(client.Transport, o.URL)`.
   - **Tests, in `internal/source/mcp_test.go`:**
     - a 302 from the endpoint becomes a 401 with the header and the
       `Siphon-Redirected` mark;
     - a 302 from another path, and a 200 from the endpoint, pass through
       unchanged.

   → verify: `go test -race ./internal/source -run 'Redirect'`.

   Traps:
   - The guarded client already refuses redirects (`netguard.go:54,59`).
     Don't touch `CheckRedirect`.
   - Compare URLs without the query and the fragment.

2. **`internal/mcpoauth/mcpoauth.go`, handler (:285-288).**
   - `Authorize` keeps returning `loginRequired(h.name)`.
   - When `resp.Header.Get("Siphon-Redirected") != ""`, it wraps that with
     "(the server redirected)" so that `errors.Is(err, ErrLoginRequired)`
     still holds.
   - **Test:** an OAuth source whose MCP endpoint always answers 302,
     reached through the steady-state `Handler` plus `source.Connect`
     (the existing `mcpOptions` path). The error wraps `ErrLoginRequired`,
     and its text doesn't contain "Found".

   → verify: `go test -race ./internal/mcpoauth -run Redirect`.

   Traps: never put a token in the error text (`cleanErr`, `clip`).

3. **`internal/mcpoauth/mcpoauth.go`, `run()` (:385-513).**
   - Add `type issuerAlias struct{ base http.RoundTripper; pin string }`
     with the logic in decision B. `x` is learned from the PRM response it
     rewrites, and it's stored on the struct under a mutex, per login.
   - In `run()`, build the discovery client:
     `disc := *hc; disc.Transport = &issuerAlias{base: hc.Transport, pin: oc.Issuer}`,
     only when `oc.Issuer != ""`. Use it for `cfg.Client` and the existing
     `GetAuthServerMetadata` call at :484.
   - Build the MCP client: `mc := *hc; mc.Transport = source.RedirectChallenge(hc.Transport, s.URL)`.
     Pass `&mc` as `StreamableClientTransport.HTTPClient` (:507).
   - Recognise the PRM response by its request path prefix
     `/.well-known/oauth-protected-resource`. Rewrite the body only on a
     200 with JSON, and set a fresh `ContentLength`.

   → verify: `go build ./... && go test -race ./internal/mcpoauth`.

   Traps:
   - **Copy the `http.Client` by value.** Never mutate the cached
     `m.client` (`mcpoauth.go:124-135`), which other logins share.
   - Read bodies through the existing `limitedTransport` cap.
   - **No new dependency.**

4. **`internal/mcpoauth/mcpoauth_test.go`: a ServiceNow-shaped fake**
   (`newSNAS`), modelled on `newAS` (:56-146).
   - It serves:
     - `POST /sncapps/mcp/x` → `302 Location: /auth_redirect.do` without a
       valid bearer;
     - the PRM at `/.well-known/oauth-protected-resource/sncapps/mcp/x` →
       `authorization_servers: [<url>/sncapps/mcp/x]`;
     - the AS metadata at
       `/.well-known/oauth-authorization-server/sncapps/mcp/x` with
       `issuer: <url>`, no `registration_endpoint`, S256, and
       `/oauth_auth.do` and `/oauth_token.do`;
     - the bare `/.well-known/oauth-authorization-server` → 404, and
       `/.well-known/openid-configuration` → 302.
   - The `/oauth_auth.do` and `/oauth_token.do` handlers are reused from
     `newAS`'s, factored into a helper.
   - **Tests:**
     - `TestServiceNowShapeLogin`: pinned P = url, a pre-registered client.
       The login completes, `tools/list` works, and the stored issuer is P.
     - `TestServiceNowShapeNeedsPin`: without the pin, the login fails with
       the issuer mismatch.
     - `TestServiceNowShapeWrongPin`: a pin on another origin, and a pin
       whose path isn't a prefix of X, each fail.
     - All of them pass `noSecrets` (:200) on errors.

   → verify: `go test -race ./internal/mcpoauth`.

   Traps:
   - Token-shaped test literals are blocked by push protection. Use
     `at-%d`-style values as the fake already does.
   - The fake uses `AllowPrivate: true`, as `newEnv` does.

5. **Docs: `docs/connections/servicenow.md`.** Add the section "Official MCP
   server (Now Assist)".
   - The OAuth app: authorization code, redirect
     `<public_url>/oauth/callback`.
   - The source YAML: `type: mcp`, `url`, and `auth.oauth` with
     `client_id`, `client_secret` (by `--secret`) and
     `issuer: https://<inst>`. Say why the issuer is required.
   - Log in on the Sources page or with `siphon connect oauth <name>`.
   - The live result from step 7, whichever way it went.
   - Then regenerate with `go generate ./docs`.

   → verify: `go test ./docs/...`.

   Traps: never put a real instance secret in the docs.

6. **Conditional, only if step 7 passes: `internal/catalog/services.yaml`
   (:354-400).**
   - Add the field
     `{key: mcp_mode, label: Agent tools, type: choice, choices: [builtin, official], default: builtin}`
     and the field `mcp_server` (text, default `sn_mcp_server_default`).
   - In `official` mode, the `{{.name}}` source renders
     `type: mcp, url: <instance>/sncapps/mcp-server/mcp/{{.mcp_server}}`
     with `auth.oauth` (`client_id`, `client_secret` and
     `issuer: <instance>`), instead of `package: servicenow`.
   - The instance field is `secure: true` today. If `url` can't take a
     `secret` value, the mode makes `instance` a plain URL field in that
     branch. Check this in `catalog_test.go` first.
   - **Tests:** a render test for each mode, and
     `go test ./internal/catalog ./docs/...`. Regenerate the schema if the
     schema test says so.

   → verify: `go test -race ./internal/catalog`.

   Traps:
   - Private hosts must be in the `private_listed` rule.
   - `UPDATE_SCHEMA=1`, if needed.

7. **Live check, on calitiiltddemo3 (no repo edit).**
   1. Build to `siphon.new` and `mv` it over siphon-dev's binary.
   2. Add an mcp source `servicenow-official` with the "Siphon MCP" app.
      The client secret goes by stdin `--secret`, from the agenix
      `SERVICENOW_MCP_CLIENT_SECRET`, parsed as data and never sourced.
   3. Start the login, and script the consent with the admin login (curl:
      a login session, the consent form, then follow to
      `127.0.0.1:8090/oauth/callback`). If it resists, ask the owner for
      one click.
   4. Then run the source Test (`tools/list`).
   5. Record the result on #104, with no token or secret in the output.

   → verify: the source shows connected, and `tools/list` returns tools,
   or there's a documented failure.

   Traps:
   - Never print the dev token or the agenix values.
   - Use the PIDs from `serve.pid`.
   - Back up `siphon.yaml` first.

## Tests

```sh
go vet ./... && go test -race ./...
```

Everything passes. `nix flake check` isn't needed: the change is
client-only, and `go test` covers the transports.

**Model split:** steps 1–4 (and step 6) edit code, so the `coder` agent does
them, one at a time. Steps 5 and 7 are mine. A fresh Opus agent reviews
`git diff` against this plan.

## Rollback

Revert the PR's commits. The behaviour only changes for `auth.oauth`
sources, and no stored state changes shape. On siphon-dev, restore the
backed-up `siphon.yaml` and the previous binary (`siphon.prev-*`), and
remove the `servicenow-official` source.
