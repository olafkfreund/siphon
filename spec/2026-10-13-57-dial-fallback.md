---
status: approved
issue: 57
intent: intent/2026-10-13-57-dial-fallback.md
---

# Spec: the guarded egress dial falls back to the next address

## Design

`guardedClient`'s `DialContext` (`internal/source/netguard.go:41-51`) keeps
its resolve-and-check step and replaces the single `ips[0]` dial with a
small unexported helper in the same file:

```go
// dialAddrs dials each checked address in turn and returns the first that connects.
func dialAddrs(ctx context.Context, d *net.Dialer, network string, addrs []netip.Addr, port string) (net.Conn, error)
```

- It tries `addrs` in resolver order and returns the first connection.
- If all of them fail, it returns the last error, the same as the loops in
  `web/models.go:152-162` and `notify/client.go`.
- It stops early when `ctx` is done, so a cancelled request doesn't try
  the remaining addresses.
- It only ever dials `netip.Addr` values that `ResolveAllowed` returned,
  never the hostname, so there's no second DNS lookup and no rebinding
  window.

`DialContext` becomes `return dialAddrs(ctx, dialer, network, ips, port)`.
Nothing else changes: not `ResolveAllowed`, not the modes, not the
redirect policy, not the body cap.

## Alternatives rejected

- **Dial the hostname with Go's own fallback** (`dialer.DialContext(ctx,
  network, address)`): this resolves again after the check, which opens a
  DNS-rebinding window. Rejected for security.
- **Happy Eyeballs (parallel IPv4/IPv6 racing):** more code, and none of
  the other five dialers do it. Sequential fallback fixes the bug.
- **One shared helper for all six dialers:** decided against at intent
  approval. The helper stays unexported in `netguard.go`.

## Risks

- **Slower failure on a host whose addresses all blackhole:** the dialer
  timeout (30 s by default) applies per address, so with N addresses a
  request can take up to N × timeout. For `HTTPClient`,
  `http.Client.Timeout` still caps the whole request. For `StreamClient`
  (no overall timeout) the per-address dial timeout is the bound. Every
  other guarded dialer already behaves this way.
- **Error text:** the error is now the last address's, not the first's.
  Job output already masks transport errors (#54), so nobody depends on
  the wording.

## Verification

- A new `TestDialAddrsFallsBack` in `internal/source/netguard_test.go`:
  - listen on `127.0.0.1:0`;
  - call `dialAddrs` with `[127.0.0.2, 127.0.0.1]` on that port (nothing
    listens on `127.0.0.2`, so Linux refuses the connection) and expect a
    connection;
  - call it with `[127.0.0.2]` alone and expect an error;
  - call it with a cancelled context and expect `ctx.Err()`, with no dial
    attempted.
- The existing `TestGuardedClientDoesNotRedirect` and
  `TestStreamClientNoRedirects` still pass.
- `go vet ./... && go test -race ./internal/source/...`, then
  `devenv test`.
- Optional: revert the IPv6 listen in `nix/stub-model.py` and run the VM
  test. With the fix, the request falls back to IPv4 instead of returning
  502. This proves the original #54 failure is fixed, but the VM test
  needs KVM.
