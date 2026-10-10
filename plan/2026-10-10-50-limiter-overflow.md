---
status: draft
issue: 50
spec: spec/2026-10-10-50-limiter-overflow.md
---

# Plan: the failed-login limiter keeps penalties under load

Self-contained. The approved decisions:

- **No more reset:** the limiter no longer replaces its map when it's
  full (`ponytail:` reset at `internal/web/web.go:405-407`).
- **A new key while the map is full (`len(l.m) >= maxBuckets`):**
  1. **Sweep:** refill every bucket, and delete those at `failBurst`
     tokens, since they carry no penalty.
  2. **Room now:** the key gets its own bucket.
  3. **Still full:** the failure is charged to the shared bucket
     `overflowKey = "*"`. The map holds at most `maxBuckets+1` entries.
- **`blocked(ip)` for a key with no bucket:**
  - while the map is full, the overflow bucket decides, if there is one;
  - otherwise the key isn't blocked, as today.
- **Unchanged:**
  - the per-key rate and the key rule (IPv4 address, IPv6 /64);
  - `trusted_proxies`;
  - the four callers (`api.go:136`, `oauth.go:100`, `portal.go:218`,
    `oidc.go:171`).
- **No background goroutine, no new dependency.** The sweep gets a
  `ponytail:` comment naming its ceiling: it's O(`maxBuckets`) per new-key
  failure while full, and the fix if that ever matters is to throttle it
  to once a second.

Below the handoff threshold (one step that edits two files), so Opus
implements it.

## Steps

1. **The limiter and its tests.**
   - **`internal/web/web.go`:**
     - Next to `failBurst`/`maxBuckets` (`:87-88`), add
       `overflowKey = "*" // shared by new keys while the map is full; never an ipKey`.
     - **`blocked`** (`:391-400`): when `l.m[ip]` is nil and
       `len(l.m) >= maxBuckets`, use `l.m[overflowKey]`. If that's nil
       too, return false.
     - **`fail`** (`:402-415`): replace the reset with the following.
       When `l.m[ip]` is nil and `len(l.m) >= maxBuckets`, sweep (refill
       each bucket, and delete the ones with `tokens >= failBurst`). If
       it's still full, set `ip = overflowKey`. Then the existing
       create-or-refill and decrement.
   - **`internal/web/web_test.go`:**
     - Replace `TestLimiterOverflowReplacesMap` (`:386-394`) with
       `TestLimiterOverflow`.
     - It uses `l := &limiter{now: func() time.Time { return now }, m: map[string]*bucket{}}`,
       with a local `now` the test moves forward.
     - **Subtests:**
       - **A penalty survives a flood:** `"a"` fails `failBurst` times,
         then `maxBuckets+10` keys fail once. `blocked("a")` is true, and
         `len(l.m) <= maxBuckets+1`.
       - **The overflow bucket is shared:** after that, `failBurst` more
         new keys fail, and a further new key is blocked. Move the clock
         2 minutes, and it isn't.
       - **Refilled buckets are swept:** a fresh limiter is filled with
         `maxBuckets` keys failing once. Move the clock 2 minutes, then a
         new key `"n"` fails. `l.m["n"]` exists, `l.m[overflowKey]`
         doesn't, and `len(l.m) == 1`.
   - **Verify:** `go vet ./... && go test -race ./internal/web`.
   - **Traps:**
     - inject the clock (`time.Now` makes the sweep subtest flaky);
     - the existing IPv6 /64 limiter test (`:375-384`) must still pass
       unchanged.

2. **Review.**
   - A fresh Opus security review gets this plan and `git diff main...`.
   - Fix the findings, and log the deviations here.

## Tests

- `go vet ./... && go test -race ./...`: all pass.
- CI (`nix flake check`, the VM test): passes unchanged, since the VM
  test's limiter subtests stay far below `maxBuckets`.

## Rollback

- Revert the merge. There's no config, data or schema change.
