---
status: approved
issue: 57
author: olafkfreund
---

# Intent: the guarded egress dial falls back to the next address

## Problem

`guardedClient` (`internal/source/netguard.go:41-51`) resolves the host,
checks every address against the private-range rules, and then dials only
`ips[0]`. When a host has several addresses and the first one can't be
reached (typically IPv6 first on a host without IPv6 routing), the request
fails, though the next address would have worked.

Every caller of `source.HTTPClient` and `source.StreamClient` is affected:
polled sources, MCP sources, and the per-run OAuth proxy from #54. #54's VM
test hit it as a 502 and worked around it in the stub
(`nix/stub-model.py` listens on `::` too).

The five other guarded dialers (`web/services.go`, `web/models.go`,
`notify/client.go`, `draft/draft.go`, `egress/proxy.go`) already try each
checked address in turn. Only this one doesn't.

## Proposed outcome

A guarded request to a host whose first address refuses or times out
succeeds over the next allowed address, as it does for the other dialers.
The address checks are unchanged: only addresses that `ResolveAllowed`
approved are ever dialed, and one blocked address still rejects the host.

## Affected users and systems

- `internal/source/netguard.go` and its test.
- Anyone running Siphon on a dual-stack or partly broken network.

## Constraints

- No change to which addresses are allowed (SSRF rules stay exactly as they are).
- Dial only the resolved and checked addresses, never the hostname again
  (no DNS rebinding window).
- The test must not need real IPv6 or the network: inject the resolver or
  use loopback listeners.

## Open questions

- Should the five copies of the "try each address" loop become one shared
  helper in `internal/source`? The smaller change fixes only `netguard.go`.
  I'd keep it to `netguard.go` and leave the copies alone.

## Decisions at approval

- Fix only `netguard.go`; the other five loops stay as they are.
