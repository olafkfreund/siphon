---
status: approved
issue: 48
spec: spec/2026-10-10-48-trusted-proxies.md
---

# Plan: trusted reverse proxies for the failed-login limiter

Self-contained. The approved decisions:

- **The setting:** `server.trusted_proxies`, a list of CIDR prefixes or
  plain addresses (/32 or /128).
  - Empty by default, which means today's behaviour.
  - Operator-only; read at start.
  - An invalid entry is a `Validate` error.
- **Header:** only `X-Forwarded-For`, and only when the TCP peer is
  trusted.
  - Walk the entries from the right, skipping trusted ones. The first
    untrusted valid address is the client.
  - A bad entry stops the walk, and the client is the last trusted hop.
  - If every entry is trusted, or there's no header, the client is the
    leftmost trusted hop.
- **The key rule is unchanged:** `Unmap`, then IPv4 as is, or the IPv6 /64.
- **Only the limiter key changes.** `secureCookie`/`X-Forwarded-Proto`,
  the audit log and `slog` are untouched. Nothing new in `go.mod`.
- **Docs:** a new `docs/tasks/reverse-proxy.md`, which the start-up warning
  points to.

## Steps

1. **Config.**
   - **Where:** `internal/config/config.go`.
     - `Server` (after `Metrics`, `:121`) gains
       `TrustedProxies []string \`yaml:"trusted_proxies"\``, with a
       comment.
     - `func (s *Server) Proxies() []netip.Prefix` parses each entry:
       `netip.ParsePrefix`, else `netip.ParseAddr` then
       `netip.PrefixFrom(a, a.BitLen())`. The result is `.Masked()`, and
       invalid entries are skipped.
     - In `Validate`, next to `c.validateOIDC(add)` (`:1118`), each bad
       entry is reported as
       `server.trusted_proxies[%d]: want an IP address or CIDR prefix`.
     - The start-up warning at `:821` adds
       `; see docs/tasks/reverse-proxy.md`.
   - **Schema:** regenerate with `UPDATE_SCHEMA=1 go test ./internal/config`.
   - **Tests:** valid entries (v4, v6, plain, prefix), an invalid one, and
     `Proxies()` masking `10.1.2.3/8` to `10.0.0.0/8`.
   - **Check:** if any test asserts the start-up warning's text, update
     it.
   - **Verify:** `go test ./internal/config`.
   - **Traps:** schema drift (commit the regenerated schema).

2. **The limiter key.**
   - **Where:** `internal/web/web.go:308-325` (`clientIP`).
     - `Options` gains `TrustedProxies []netip.Prefix`, read at start.
     - `clientIP(r)` becomes the method `s.clientIP(r)`. Keep the key rule
       in a helper, `ipKey(netip.Addr) string`.
     - `trusted(a)` is true when a prefix contains `a.Unmap()`.
     - The walk works as the decisions above say. `r.Header.Values("X-Forwarded-For")`
       is joined and then split on `,`, with each entry trimmed. An entry
       may carry a port (`1.2.3.4:5`, `[::1]:5`), so try
       `netip.ParseAddrPort` before `netip.ParseAddr`.
     - A `RemoteAddr` that doesn't parse (a unix socket) keeps today's
       return of the raw host.
     - Replace the `ponytail:` comment, because it no longer holds.
   - **Callers:** `api.go:135`, `oauth.go:99`, `portal.go:217` and
     `oidc.go:170`, found with `grep -n 'clientIP(' internal/web/*.go`.
   - **`cmd/siphon/main.go:507`** passes
     `TrustedProxies: cfg.Server.Proxies()`.
   - **Tests** (`internal/web/proxy_test.go`):
     - **A table** over `s.clientIP` covering:
       - no trusted proxies;
       - an untrusted peer with a header;
       - a trusted peer with `c`;
       - `spoof, c`;
       - a chain `c, p2` with `p2` trusted;
       - a bad entry, which gives the last trusted hop;
       - every entry trusted, which gives the leftmost;
       - no header, which gives the peer;
       - IPv6 (the /64);
       - an IPv4-mapped peer `[::ffff:127.0.0.1]`;
       - two header lines;
       - entries with ports.
     - **A limiter test:** behind a trusted peer, `failBurst` bad
       `POST /login` from forwarded client A, then A gets 429 and
       forwarded client B gets 401, not 429. Use the injected clock.
   - **Verify:** `go vet ./... && go test -race ./internal/web ./cmd/siphon`.
   - **Traps:**
     - inject the clock;
     - tests that set `RemoteAddr` still pass, because no proxies are
       configured there.

3. **The VM test** (Opus).
   - **Where:** `nix/vm-test.nix`.
     - The server block (near `oidc`, `:114`) adds
       `trusted_proxies = [ "127.0.0.1" ];`.
     - A new subtest after the SSO one (`:971`):
       - 5 bad `POST /login` with `-H 'X-Forwarded-For: 203.0.113.7'`;
       - a sixth from the same client gets 429;
       - a good token login with `-H 'X-Forwarded-For: 203.0.113.8'`
         sets the cookie.
   - **Check:** `curl` without the header now keys on `127.0.0.1` (the
     trusted peer, with no header), so the existing subtests' budget is
     unaffected.
   - **Verify:** `nix flake check`.
   - **Traps:**
     - the test script is type-checked;
     - write any `systemctl` text with the Write tool.

4. **Docs** (Opus).
   - **The new `docs/tasks/reverse-proxy.md`:**
     - why to use TLS;
     - Caddy and nginx examples that set `X-Forwarded-For` and
       `X-Forwarded-Proto`;
     - `server.public_url`;
     - `server.trusted_proxies`, with the warning never to list a range
       that clients can connect from directly;
     - the NixOS settings path.
   - **Also:** a `docs/README.md` row, and `go generate ./docs` for
     `llms-full.txt`.
   - **Verify:** `go test ./docs/`.
   - **Traps:** no vendor logos.

5. **Review.**
   - A fresh Opus security review gets this plan and `git diff main...`.
   - Fix the findings, and log the deviations here.

## Tests

- `go vet ./... && go test -race ./...`: all pass.
- `nix flake check`: passes, including the new subtest.

## Rollback

- **Revert the merge,** after removing `server.trusted_proxies` from
  `siphon.yaml`, since the older version rejects the key.
- **No data changes.**

## Deviations

None yet.
