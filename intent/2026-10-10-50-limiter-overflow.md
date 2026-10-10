---
status: approved
issue: 50
author: olafkfreund
---

# Intent: the failed-login limiter must not forget penalties under load

## Problem

The failed-login limiter (`internal/web/web.go`, `limiter.fail`) tracks one
bucket per client key: an IPv4 address or an IPv6 /64. It allows 5 failures
a minute per key. When the map holds more than `maxBuckets` (4096) keys,
`fail` replaces the whole map, and every penalty is forgotten, including
the attacker's own.

A client that controls more than 4096 keys can therefore reset its own
lockout whenever it likes. IPv6 makes this cheap: a single /48 holds 65,536
/64s. Each reset buys 5 more guesses, so the limit on these is effectively
gone:

- the token login;
- SSO;
- the API and the CLI;
- the MCP OAuth callback.

The #48 security review found this. It predates #48.

## Proposed outcome

- **Under a flood of new keys, a penalty is kept until it expires.** A key
  that's locked out stays locked out until its bucket refills (about a
  minute), however many other keys fail meanwhile.
- **Memory stays bounded,** as it is today (`maxBuckets`).
- **Normal use doesn't change:** 5 failures a minute per key, and the same
  IPv4 and /64 keys, behind a trusted proxy or not.
- **A test shows it:** a locked-out key stays at 429 after more than
  `maxBuckets` other keys have failed.

## Affected users and systems

- `internal/web/web.go` (the limiter) and its tests.
- Every surface that uses the limiter: the portal login, SSO, the API,
  and the MCP OAuth callback.
- No config, schema, NixOS module or migration changes. No hosts.

## Constraints

- **Must:**
  - stay bounded in memory;
  - keep the lock cheap, since the limiter is on every failed login;
  - inject the clock in tests.
- **Must not:**
  - change the per-key rate, the key rule or `trusted_proxies`;
  - add a dependency;
  - give a flood a way to lock out every user indefinitely. Some shared
    limit for new keys during a flood is acceptable. A permanent global
    lockout isn't.

## Open questions

1. **When the map is full of live penalties,** that is, more than 4096 keys
   failed in the last minute, what happens to a new key? Options for the
   spec:
   - **(a)** All new keys share one overflow bucket until space frees up.
     It fails closed, but honest new clients may see 429 during a flood.
   - **(b)** Evict the oldest penalty. Memory stays bounded, but an
     attacker with enough keys can still push its own penalty out.

   I'd propose (a), after first dropping buckets that have refilled. Those
   carry no penalty, so forgetting them is free.

## Resolved

1. **(a)**, the proposal. Buckets that have refilled are dropped first,
   then new keys share one overflow bucket while the map is still full.
