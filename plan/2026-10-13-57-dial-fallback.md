---
status: approved
issue: 57
spec: spec/2026-10-13-57-dial-fallback.md
---

# Plan: the guarded egress dial falls back to the next address

## Approved decisions

- `guardedClient`'s `DialContext` (`internal/source/netguard.go:41-51`)
  dials each address `ResolveAllowed` returned, in resolver order, and
  returns the first connection that succeeds.
- If every address fails, it returns the last error. If `ctx` is done, it
  stops and returns `ctx.Err()`.
- It only dials checked `netip.Addr` values, never the hostname again, so
  there's no DNS-rebinding window.
- The loop is a new unexported `dialAddrs` in `netguard.go`. The other five
  dialers (`web/services.go`, `web/models.go`, `notify/client.go`,
  `draft/draft.go`, `egress/proxy.go`) are not touched.
- No change to `ResolveAllowed`, the modes, redirects or the body cap.
- Known and accepted: with N addresses that all blackhole, a dial can take
  N × the dialer timeout. `http.Client.Timeout` caps `HTTPClient`.
  `StreamClient` relies on the per-address dial timeout.

## Steps

1. **`internal/source/netguard_test.go`: write the failing test first.**
   Add `TestDialAddrsFallsBack`:
   - `l, _ := net.Listen("tcp", "127.0.0.1:0")`, `defer l.Close()`, and
     a goroutine that accepts and closes connections;
   - take `port` from `l.Addr()`, and use `d := &net.Dialer{Timeout: 2 * time.Second}`;
   - `dialAddrs(ctx, d, "tcp", []netip.Addr{127.0.0.2, 127.0.0.1}, port)`
     → no error, and a non-nil conn, which you close;
   - `[127.0.0.2]` alone → an error;
   - a cancelled context with `[127.0.0.1]` → `errors.Is(err, context.Canceled)`.

   Add `errors` to the imports.

   → Verify: `go test ./internal/source/ -run TestDialAddrsFallsBack`
   fails to compile (`dialAddrs` is undefined).

   Traps: nothing listens on 127.0.0.2, and Linux answers with RST
   (refused) at once, so no timeout is involved. No real clock waits. No
   unix sockets.

2. **`internal/source/netguard.go`: add `dialAddrs` after `guardedClient`.**

   ```go
   // dialAddrs dials each checked address in turn and returns the first that connects.
   func dialAddrs(ctx context.Context, d *net.Dialer, network string, addrs []netip.Addr, port string) (net.Conn, error) {
   	var last error
   	for _, a := range addrs {
   		if err := ctx.Err(); err != nil {
   			return nil, err
   		}
   		conn, err := d.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
   		if err == nil {
   			return conn, nil
   		}
   		last = err
   	}
   	return nil, last
   }
   ```

   Replace line 50 with `return dialAddrs(ctx, dialer, network, ips, port)`.

   → Verify: `go test -race ./internal/source/...` passes, including the
   new test, `TestGuardedClientDoesNotRedirect` and
   `TestStreamClientNoRedirects`.

   Traps: `ResolveAllowed` never returns an empty slice without an error
   (`netguard.go:92-94`), so `last` is never nil on failure.

3. **Whole repo.**
   → Verify: `go vet ./... && go test -race ./...` (or `devenv test`).
   Traps: none.

## Tests

- `go test -race ./internal/source/...`: the new test plus the existing
  ones pass.
- `go vet ./... && go test -race ./...`: green.
- Optional, needs KVM: revert the IPv6 listen in `nix/stub-model.py`
  locally, without committing it, and run `nix build .#checks.x86_64-linux.vm`.
  It passes because the dial falls back to IPv4.

## Rollback

Revert the commit. It touches only `netguard.go` and its test.
