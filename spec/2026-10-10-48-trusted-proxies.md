---
status: approved
issue: 48
intent: intent/2026-10-10-48-trusted-proxies.md
---

# Spec: trusted reverse proxies for the failed-login limiter

## Design

### Config

```yaml
server:
  trusted_proxies: [127.0.0.1, 10.0.0.0/8, "fd00::/8"]
```

- **The field:** `Server.TrustedProxies []string`
  (`yaml:"trusted_proxies"`), in `internal/config/config.go`.
- **An entry** is a CIDR prefix (`netip.ParsePrefix`) or a plain address
  (`netip.ParseAddr`, which becomes /32 or /128).
- **`Validate`** reports each entry that is neither:
  `server.trusted_proxies[i]: want an IP address or CIDR prefix`.
- **A method,** `(*Server).Proxies() []netip.Prefix`, returns the parsed,
  masked prefixes. Invalid entries are skipped, since `Validate` has
  already reported them.
- **Operator-only.** It's under `server.*`, so the overlay rules already
  keep it out of portal and API edits (`overlay.go` compares all of
  `Server`).
- **Read at start,** like `server.token`. A change needs a restart.
- **Schema:** regenerated. `siphon explain` has no `server` kind, so it
  doesn't change.

### The limiter key (`internal/web`)

- **`Options.TrustedProxies []netip.Prefix`.** `cmd/siphon/main.go:507`
  passes `cfg.Server.Proxies()`.
- **`clientIP(r)`** becomes `s.clientIP(r)`, at all four callers:
  `api.go:135`, `oauth.go:99`, `portal.go:217` and `oidc.go:170`.
  1. **The peer** is the address from `r.RemoteAddr`. Without any trusted
     proxies, or when the peer isn't in one, the peer is the client. That
     is today's behaviour.
  2. **When the peer is trusted,** read every `X-Forwarded-For` header
     value, split on commas and trim each entry. Walk the entries from the
     **right**:
     - a trusted entry is skipped (a chain of proxies);
     - the first untrusted, valid address is the client;
     - an entry that isn't an address stops the walk, and the last trusted
       hop is the client: fail closed, without trusting the bad text;
     - when every entry is trusted, or there is no header, the leftmost
       trusted hop seen is the client: usually the peer itself.
  3. **The existing key rule** is applied to the result: `Unmap`, then the
     IPv4 address, or the IPv6 /64.
- **Why from the right:** a client can put anything at the left of the
  header, and each proxy appends the address it saw. Only the entries
  added by trusted proxies are believable, so the first untrusted one
  from the right is the furthest-out address a trusted proxy vouched for.
- **The header is only for the limiter key.** The audit log, `slog`,
  `secureCookie` and every other part of Siphon are unchanged.

### Docs

- **The new how-to, `docs/tasks/reverse-proxy.md`:**
  - why to put Siphon behind TLS (the start-up warning points here);
  - minimal nginx and Caddy examples that set `X-Forwarded-For` and
    `X-Forwarded-Proto`;
  - `server.public_url`;
  - `server.trusted_proxies`, and what goes wrong without it (one shared
    limiter bucket);
  - the warning: never list a range that untrusted clients can connect
    from.
- **Also:** a `docs/README.md` row, and the regenerated `llms-full.txt`.
- **The start-up warning** (`config.go:821`) adds "see
  docs/tasks/reverse-proxy.md".

## Alternatives rejected

- **Trust `X-Forwarded-For` whenever the listener isn't loopback.** Any
  client that reaches Siphon directly could then pick its own bucket. That
  turns lockout protection into no protection.
- **Take the leftmost entry.** It's what the client claims, so it's
  spoofable through any proxy that appends rather than replaces.
- **Also parse RFC 7239 `Forwarded`.** Rejected at the intent (question 2).
- **A hop count instead of a list.** It's fragile: a CDN in front, or a
  proxy that's added later, silently shifts the address used.

## Risks

- **A list that's too wide.** Listing a range that clients can also
  connect from directly lets them choose their own bucket, and so get
  unlimited guesses. The docs warn about this. The default is an empty
  list, which is today's behaviour.
- **A proxy that doesn't set the header.** The key falls back to the
  proxy's address: one shared bucket, the same as today. Nothing is worse.
- **Hosts.** None are changed. Operators opt in through config.

## Verification

- **Unit tests,** `internal/web`, table-driven, for `s.clientIP`:
  - no trusted proxies: the header is ignored;
  - an untrusted peer with a header: the header is ignored;
  - a trusted peer with `client`: the client;
  - a trusted peer with `spoof, client`: the client;
  - a chain, `client, proxy2` with `proxy2` trusted: the client;
  - a bad entry: the last trusted hop;
  - every entry trusted: the leftmost;
  - no header: the peer;
  - IPv6: the /64;
  - an IPv4-mapped IPv6 peer, and several header lines.
- **A limiter test:** behind a trusted peer, 5 failures from one forwarded
  client block it but not a second forwarded client.
- **`internal/config`:** valid and invalid entries, and `Proxies()`
  masking.
- **VM test:** set `trusted_proxies = [ "127.0.0.1" ]`. With an
  `X-Forwarded-For` client, burn that client's budget against
  `POST /login`, then show that the token login still works for a
  different forwarded client.
- **Builds:** `go vet ./... && go test -race ./...`, then
  `nix flake check`.
