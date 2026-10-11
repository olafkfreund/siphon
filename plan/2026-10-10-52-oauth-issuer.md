---
status: draft
issue: 52
spec: spec/2026-10-10-52-oauth-issuer.md
---

# Plan: pin an MCP source's authorization server (`auth.oauth.issuer`)

Self-contained. The approved decisions:

- **Config:** `OAuth.Issuer string` (`yaml:"issuer"`), optional.
  - When set, it must be `https`, or `http` on a loopback host, the same
    rule as `server.oidc.issuer`.
  - Anyone who can edit the source can set it (portal/API/`apply`); it
    isn't secret.
  - `Warnings()` flags `client_secret` set without `issuer`.
- **With a pin, a login through any other authorization server fails**
  before any secret, registration or browser redirect reaches that
  server:
  - **Preregistered `client_id`:** go-sdk does the check when
    `ClientCredentials.Issuer` is set (`auth/authorization_code.go:558`),
    and Siphon sets it from the pin.
  - **Dynamic registration:** go-sdk doesn't check on that path, so
    Siphon registers itself against the pinned server
    (`auth.GetAuthServerMetadata`, then `oauthex.RegisterClient`). It
    then passes the result as a `PreregisteredClient` with `Issuer` set
    to the pin. The stored login stays `Dynamic: true`.
  - **Without a pin:** unchanged. Preregistered clients keep trust on
    first use, and go-sdk does the dynamic registration.
- **Stored logins:**
  - `stored.Issuer` is the pin when one is set, otherwise `l.iss` as
    today.
  - With a pin, `load` refuses a stored login whose `Issuer` differs,
    comparing with the trailing `/` trimmed. An empty `Issuer` counts as
    different, so a re-login is needed.
- **Errors:** go-sdk's mismatch error names both issuers, which are URLs
  only. It goes through the existing `cleanErr`. Nothing new is logged.
- **No new dependency.** No migration.

## Steps

1. **Config** (coder).
   - **Where:** `internal/config/config.go`.
     - `OAuth` (`:364-368`) gains `Issuer string \`yaml:"issuer"\``, with
       a comment.
     - Add `func secureURL(s string) bool`: it parses `s`, and is true
       when the host is non-empty and the scheme is `https`, or `http`
       with `loopbackListen(net.JoinHostPort(host, "0"))`. Use it in
       `validateOIDC` (`:199-202`, same behaviour) and in `validateOAuth`
       (`:957-980`). When `o.Issuer != "" && !secureURL(o.Issuer)`, the
       error is `%s.auth.oauth.issuer: must be https, or http on a loopback host`.
     - In `Warnings()`, in the per-source loop (`:855-`): when
       `s.Auth != nil && s.Auth.OAuth != nil && s.Auth.OAuth.ClientSecret.isSet() && s.Auth.OAuth.Issuer == ""`,
       add `source %s: auth.oauth.client_secret without auth.oauth.issuer: the first login trusts whichever authorization server the MCP server names`.
       (Check that `isSet` is right for `Secret`, as `:970` uses it.)
   - **`internal/config/explain.go:55-58`:** add
     `"auth.oauth.issuer": {false, "", "oauth: pin the authorization server; a login through any other fails. Set it with a client_secret, or for an MCP server you don't run", nil}`.
   - **Schema:** `UPDATE_SCHEMA=1 go test ./internal/config`, then commit
     `schema/siphon.schema.json`.
   - **Tests** (`internal/config/config_test.go`, next to the existing
     `auth.oauth` validation tests):
     - `issuer` with `ftp://x`, `http://example.com` and `not a url`
       fails;
     - `https://login.example.com` and `http://127.0.0.1:9000` pass;
     - the warning appears with a `client_secret` and no `issuer`, but
       not with both, and not without a secret;
     - `server.oidc.issuer` tests still pass unchanged.
   - **Verify:** `go vet ./... && go test ./internal/config`.
   - **Traps:** schema drift (commit the regenerated file); if any
     explain test lists the fields, update it.

2. **Login and reuse** (coder).
   - **Where:** `internal/mcpoauth/mcpoauth.go`.
     - **`load` (`:137-149`):** after the client-id check, if
       `p := s.Auth.OAuth.Issuer; p != "" && strings.TrimSuffix(st.Issuer, "/") != strings.TrimSuffix(p, "/")`,
       return `nil, stored{}, false`.
     - **In `run`, `NewTokenSource` (`:421`):** `Issuer: l.iss` becomes
       the pin when `oc.Issuer != ""`, otherwise `l.iss`.
     - **The registration `switch` (`:450-470`):**
       - **`case oc.ClientID != ""`:** after the existing
         `if reuse { pc.Issuer = prev.Issuer }`, set
         `if oc.Issuer != "" { pc.Issuer = oc.Issuer }`.
       - **The `reuse && prev.Issuer != ""` case:** unchanged, since
         `load` already guarantees a match.
       - **A new case before `default`, `oc.Issuer != ""`** (a new
         dynamic client with a pin):
         - `asm, err := auth.GetAuthServerMetadata(ctx, oc.Issuer, hc)`.
           If `err != nil || asm == nil || asm.RegistrationEndpoint == ""`,
           return `fmt.Errorf("authorization server %s does not support dynamic registration: set auth.oauth.client_id", oc.Issuer)`,
           wrapped through `cleanErr` if `err` is non-nil.
         - `reg, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, <the same ClientRegistrationMetadata as default>, hc)`.
           Factor that metadata literal into a local, so `default` and the
           new case share it.
         - Then `cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: reg.ClientID, Issuer: oc.Issuer}`,
           plus `ClientSecretAuth` when `reg.ClientSecret != ""`.
       - **Check that `Dynamic: oc.ClientID == ""` (`:422`) still holds**
         (it's true here) and that `st.ClientSecret = c.ClientSecret` is
         stored, so the next login reuses this client through the
         `reuse && prev.Issuer != ""` case.
     - **Check the `run` signature:** whether `ctx` is in scope where the
       switch runs. If it isn't, use the context `run` was given.
   - **Tests** (`internal/mcpoauth/mcpoauth_test.go`):
     - **`fakeAS`** gains a `tokens int` counter, incremented in its
       `/token` handler.
     - **A second server, `b := newAS(t)`,** serves as "the other
       authorization server". `newEnv` builds the MCP source on `as`.
     - **`TestIssuerPinPreregistered`:**
       - pin = `b.URL`, `client_id` and secret set, so the MCP server
         names `as`. The login fails, the error names both issuers and
         not the secret (`noSecrets`), and `as.tokens == 0`;
       - pin = `as.URL`, with `as.issuerSent` making the callback carry
         no `iss`, if the harness allows that (otherwise the real one).
         The login succeeds, and `stored.Issuer == as.URL`.
     - **`TestIssuerPinDynamic`:**
       - pin = `b.URL`, with no `client_id`. The login fails,
         `as.registers == 0` and `as.tokens == 0`. Registration went to
         `b`, so `b.registers == 1`;
       - pin = `as.URL`. The login succeeds, `as.registers == 1`, and
         the stored login is `Dynamic`. A second login reuses the client,
         so `as.registers` stays 1.
     - **`TestIssuerPinRefusesOldLogin`:** log in with no pin, then set
       `Issuer = as.URL + "/"`.
       - If the stored issuer equals `as.URL`, `Status` stays logged in
         (the trailing `/` is ignored).
       - With pin = `b.URL`, `Status` is `"none"`.
       - Use a stored login whose `Issuer` is `""` (rewrite the file) to
         show that an empty one is refused when a pin is set.
   - **Verify:** `go vet ./... && go test -race ./internal/mcpoauth ./internal/config`.
   - **Traps:**
     - never put a secret in an error (`noSecrets`);
     - the fake servers are on 127.0.0.1, so they pass `secureURL`, and
       `AllowPrivate` is already set;
     - inject the clock (the existing `clock`).

3. **Docs** (Opus).
   - **`docs/tasks/connect-oauth-mcp.md`:**
     - add `issuer:` to the preregistered example (`:41-47`), and a
       bullet "Pin the authorization server" covering:
       - when to set it: a `client_secret`, or an MCP server you don't
         run;
       - where the value comes from: the `issuer` in
         `<authorization server>/.well-known/oauth-authorization-server`;
       - that it works with dynamic registration too.
     - in "When the login stops working" (`:90`): add `issuer`. Note that
       adding a pin can need one re-login.
   - **Also:** `go generate ./docs` for `llms-full.txt`.
   - **Verify:** `go test ./docs/`.

4. **Review.**
   - A fresh Opus security review gets this plan and `git diff main...`.
   - Fix the findings, and log the deviations here.

## Tests

- `go vet ./... && go test -race ./...`: all pass.
- CI (`nix flake check`): passes. The VM test's OAuth subtest doesn't set
  `issuer`, so it's unchanged.

## Rollback

- **Revert the merge,** after removing any `auth.oauth.issuer` from the
  config, since the older version rejects the key.
- **Stored logins** made under a pin stay readable by the older version,
  since `Issuer` already exists in `stored`.
