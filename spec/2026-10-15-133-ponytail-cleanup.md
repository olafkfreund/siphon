---
status: draft
issue: 133
intent: intent/2026-10-15-133-ponytail-cleanup.md
---

# Spec: remove duplicated helpers and dead code

## Design

Apply the 15 audit findings (listed in full in the plan) in four groups,
lowest risk first. Each group is a separate commit, so a bisect can isolate
it.

1. **Pure stdlib swaps, no logic change.**
   - Sorted keys becomes `slices.Sorted(maps.Keys(m))`. The 5 helpers go,
     and so do the 14 inline loops.
   - `orDefault`, `firstNonEmpty` and `firstOf` become `cmp.Or`.
   - The dead `rand.Read` error branches go. Go 1.24+ documents that it
     never returns an error, and go.mod is 1.26.
   - `cmd/siphon/draft.go` and `tasks.go` use `catalog.NewSecret`.
2. **One copy of each small helper:**
   - one generic nil-to-empty, `config.NonNil[T]`;
   - one clip, the exported `source.ClipRunes`;
   - one kind list, an ordered exported slice in internal/config used by
     applyfile, cmd and web;
   - one header RoundTripper;
   - the pass-through wrappers removed, with callers calling the target;
   - the 3 dead functions removed, with their tests pointed at the real
     paths;
   - `web.applyItem` embeds `applyfile.Item`;
   - `action.decodeJSON` replaced by `source.DecodeJSON`.
3. **Logic consolidation:**
   - `config.StepNeedsApproval` takes the resolved `*Agent`, so job and web
     share it;
   - `source.Limiter` uses `rate.Limiter`, with the clock passed through
     `AllowN(now, 1)` so the injected-clock tests still hold.
4. **Security code** (the riskiest group, last):
   - the 4 guarded HTTP clients (notify, draft, `listModels`,
     `runHTTPTest`) use `source.HTTPClient(cfg.ServiceEndpoint(u) or
     PrivateEndpoint, timeout, max)`. Each keeps its URL and scheme checks
     and its current private-host policy;
   - the 5 loopback checks become one `config.Loopback(host)`, with
     identical semantics: `localhost`, or an IP that `IsLoopback()`.

## Alternatives rejected

- **Leaving the security duplicates:** four copies of the dial guard means
  four places to fix the next SSRF bug. One shared, tested guard is safer.
- **A generic `sets`/`maps` package of our own:** the stdlib already has
  these.

## Risks

- **Group 4 could widen or narrow a host policy.** Mitigation: before
  editing, list each caller's current policy (public only, or private
  without link-local) in the plan's Deviations or notes. The netguard,
  security and notify tests must pass unchanged. The review checks each
  call site's policy argument.
- **`rate.Limiter` refill differs slightly** from the hand-written bucket at
  fractional rates. The existing limiter tests decide; if one fails, keep
  the hand-written bucket and log it.
- **Merge conflicts with #132 and the bug fixes.** Implement from main after
  they merge.

## Verification

- `go vet ./... && go test -race ./...`, with tests unchanged except those
  of the deleted dead functions.
- `staticcheck -checks U1000 ./...` (or `deadcode`) reports nothing new.
- Lines removed counted with `git diff --stat`, aiming for about -400.
- An Opus review that checks the group 4 policy arguments one by one.
