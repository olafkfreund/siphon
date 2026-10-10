---
status: approved
issue: 48
author: olafkfreund
---

# Intent: trusted reverse proxies for the failed-login limiter

## Problem

Siphon limits failed sign-ins per client address: 5 a minute
(`clientIP`, `internal/web/web.go:311`). It reads the address from the TCP
connection only.

**Behind a reverse proxy,** every request comes from the proxy's address,
so every user shares one bucket. Siphon's own start-up warning tells
operators to use a reverse proxy when the portal isn't on loopback, so
this is the normal setup. The result:

- **Anyone can lock everyone out.** Five bad requests a minute keep the
  shared bucket empty, and then every user gets 429 on:
  - the token login;
  - SSO;
  - the API and the CLI;
  - the MCP OAuth callback.
- **The audit trail loses the attacker.** The limiter can't tell one
  client from another, so brute-forcing isn't slowed per attacker.

The #44 and #46 security reviews both raised this.

## Proposed outcome

- **An operator names the proxies Siphon trusts.** Behind one of them, the
  limiter keys on the real client address. Each user then has their own
  bucket, and an attacker only locks themselves out.
- **Unchanged without the setting.** Siphon trusts no forwarding header,
  exactly as today.
- **No spoofing.** A client that connects directly can't pick its own
  address with a header.

## Affected users and systems

- **Code:** `internal/web` (`clientIP`, its 4 callers), `internal/config`
  (one `server.*` setting, `Validate`, the schema).
- **Docs:** a reverse-proxy section, wherever the TLS/proxy advice
  belongs, and `llms-full.txt`.
- **Operators** who run Siphon behind nginx, Caddy, Traefik or a load
  balancer, and set the option. Nobody else.
- **No host changes,** and no NixOS module change beyond the setting
  passing through `settings`.

## Constraints

- **Off by default.** No forwarding header is trusted unless configured.
- **Operator-only,** in `siphon.yaml`, like all of `server.*`. The portal
  and the API can't change it.
- **Trust only the named proxies.** Read forwarding headers only when the
  connection comes from a trusted address. Walk `X-Forwarded-For` from the
  right and stop at the first address that isn't a trusted proxy, so a
  client can't prepend a fake one.
- **Keep the IPv6 rule:** a /64 is one bucket, whatever the source of the
  address.
- **No new dependency.**

## Open questions

1. **How are proxies named?** As CIDR prefixes (`10.0.0.0/8`,
   `127.0.0.1/32`), or as single addresses only?
   Recommendation: CIDR prefixes, where a plain address means /32 or /128.
   Load balancers often sit in a subnet.
2. **Which header?** `X-Forwarded-For`, the RFC 7239 `Forwarded` header,
   or both?
   Recommendation: `X-Forwarded-For` only. Every common proxy sets it, and
   two parsers are twice the surface.
3. **Anything else from the proxy?** `secureCookie` already trusts
   `X-Forwarded-Proto` from anyone (`web.go:375`). That's harmless: a
   spoofed value only makes a cookie `Secure`.
   Recommendation: leave it, and don't widen this task. The limiter key is
   the only change.
4. **Use the real address anywhere else,** such as the audit log or logs?
   Recommendation: not now. Audit rows carry actors, not addresses.

## Resolved (owner approval, 2026-10-10)

The owner approved every recommendation:

1. **Proxies** are CIDR prefixes; a plain address means /32 or /128.
2. **Header:** `X-Forwarded-For` only.
3. **`X-Forwarded-Proto`** is left as it is; the limiter key is the only
   change.
4. **The real address** is used only for the limiter, not the audit log or
   the logs.
