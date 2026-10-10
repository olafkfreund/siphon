---
status: draft
issue: 50
intent: intent/2026-10-10-50-limiter-overflow.md
---

# Spec: the failed-login limiter keeps penalties under load

## Design

All changes are in `internal/web/web.go`, in `limiter` (`:373-415`). The
callers (`api.go:136`, `oauth.go:100`, `portal.go:218`, `oidc.go:171`)
keep calling `blocked(ip)` and then `fail(ip)`, unchanged.

### `fail(ip)`

- **A key that already has a bucket** works as today: refill, then
  subtract one.
- **A new key while the map holds fewer than `maxBuckets` keys** gets its
  own bucket, as today.
- **A new key while the map is full:**
  1. **Sweep:** refill every bucket, and delete the ones back at
     `failBurst`. A full bucket carries no penalty, so a fresh bucket
     behaves the same and nothing is lost by forgetting it.
  2. **If there's room now,** the key gets its own bucket.
  3. **Otherwise,** the failure is charged to one shared bucket under a
     reserved key, `overflowKey = "*"`.
     - No client key can collide with it, since `ipKey` always gives an
       address.
     - That bucket is the only one allowed past `maxBuckets`, so the map
       holds at most `maxBuckets+1` entries.
- **The old line goes:** the `ponytail:` map reset (`:405-407`) is
  removed.

### `blocked(ip)`

- **A key with a bucket:** as today.
- **A key with no bucket, while the map is full:** the overflow bucket
  decides, if it exists.
  - This makes `blocked` agree with `fail`: a new key that failed through
    the overflow bucket is held by it.
- **A key with no bucket, with room in the map:** not blocked, as today.
  - A leftover overflow bucket doesn't affect new keys once there's room.

### What changes for users

- **Normal use:** nothing. Under 4096 keys failing within about a minute,
  every key keeps its own 5-a-minute limit.
- **During a flood** of more than 4096 keys failing within about a minute:
  - every key that already has a bucket keeps its own limit and its own
    penalty;
  - new keys share 5 failures a minute between them;
  - honest new users may see 429 until the flood drops and the sweep makes
    room. That's about a minute after it stops.

## Alternatives rejected

- **Evict the oldest penalty (option b):** an attacker with enough keys
  pushes its own penalty out. Rejected at the intent.
- **Keep the reset, raise `maxBuckets`:** a bigger number to reach, but the
  same reset. IPv6 makes any number cheap to reach.
- **Refuse every new key when full** (global 429): that lets a flood lock
  everyone out for its whole duration. The overflow bucket already does
  that, but only for new keys, and only at 5 a minute.
- **A background sweeper goroutine:** it adds a lifecycle to manage. A
  sweep on demand, under the existing lock, runs only when the map is
  full.

## Risks

- **Sweep cost:**
  - Each sweep is O(`maxBuckets`), about 4096 map entries (tens of
    microseconds), under the lock.
  - It only runs when a new key fails while the map is full.
  - During a sustained flood of live penalties, every new-key failure
    sweeps. That cost is bounded per request, and the attacker already
    pays for a TCP connection and an HTTP request each time. The code
    gets a `ponytail:` comment naming the ceiling: throttle the sweep
    (once a second) if profiling ever shows it.
- **A shared limit for new users during a flood:** accepted at the
  intent, since it fails closed.
- **Hosts:** none changed.

## Verification

Unit tests in `internal/web/web_test.go`, using an injected clock. They
replace `TestLimiterOverflowReplacesMap`.

- **A penalty survives a flood:**
  - key A fails `failBurst` times;
  - then `maxBuckets+10` other keys fail once each, at the same instant;
  - A is still blocked.
- **Memory stays bounded:** after that, `len(l.m) <= maxBuckets+1`.
- **The overflow bucket is shared:**
  - with the map full of live buckets, `failBurst` new keys fail once each;
  - a further new key is then blocked;
  - after the clock moves 2 minutes, that key isn't blocked.
- **Refilled buckets are swept:**
  - fill the map, then move the clock 2 minutes;
  - a new key's failure gets its own bucket, not the overflow bucket;
  - the stale buckets are gone.
- **Builds:** `go vet ./... && go test -race ./...`, then CI's
  `nix flake check`.
