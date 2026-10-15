---
status: approved
issue: 133
spec: spec/2026-10-15-133-ponytail-cleanup.md
---

# Plan: remove duplicated helpers and dead code

## Approved decisions

- **No behaviour change.** Existing tests stay unchanged, except tests of
  the 3 deleted dead functions, which are pointed at the real paths. The
  only dependency change promotes `golang.org/x/time` from indirect to
  direct.
- **Four groups, one commit each, lowest risk first.**
- **Untouched:** secret masking, CSP code, the agentloop CONNECT dialer, and
  the local `jobs ls`/`approve -config` path.
- **Group 4 keeps each caller's host policy exactly.** If `rate.Limiter`
  fails an existing limiter test, keep the hand-written bucket and log it.
- **Start from main after #131, #132 and the bug-fix issue merge.** The
  line numbers below are from main at 675db3a; search by name.

## Steps

1. **Group 1, stdlib swaps.** Audit items 1, 6 and 8:
   - `slices.Sorted(maps.Keys(m))` replaces the sorted-keys helpers and
     loops;
   - `cmp.Or` replaces `orDefault`, `firstNonEmpty` and `firstOf`;
   - the `rand.Read` error branches go;
   - `catalog.NewSecret` replaces the inline copies in `cmd/siphon/draft.go`
     and `tasks.go`.

   Verify: `go vet ./... && go test -race ./...`.
   Traps: a loop that also filters or transforms is not a pure sort; leave
   it. Keep `sort.Strings` where a slice, not a map, is sorted.
2. **Group 2, one copy each.** Audit items 3, 4, 7, 9, 10, 12, 13 and 15:
   - `config.NonNil[T]`;
   - `source.ClipRunes`, exported;
   - one ordered kind slice in internal/config;
   - one `headerTransport`;
   - the pass-through wrappers inlined;
   - the dead functions deleted (`emptyIfNil`, `writeSecret`, `OrGET`), and
     their tests moved to the real paths;
   - `web.applyItem` embeds `applyfile.Item`;
   - `source.DecodeJSON` used in `internal/action`.

   Verify: the same, plus `staticcheck -checks U1000 ./...` if available.
   Traps: a clip helper may count bytes or runes differently, so check each
   caller's expectation before replacing it, and keep the rune-safe one.
   Kind order matters for output: keep the current order.
3. **Group 3, logic.** Audit items 14 and 5:
   - `config.StepNeedsApproval(st Step, a *Agent)`, used by `job.routine.go`
     and web;
   - `source.Limiter` wraps `*rate.Limiter`, keeps the same API, and takes
     the clock through `AllowN(now, 1)`.

   Verify: `go test -race ./internal/source ./internal/job ./internal/web`,
   then everything, then
   `nix build --rebuild .#siphon.goModules` (go.mod changed).
   Traps: after a go.mod change, update `vendorHash` and confirm it with
   `--rebuild`. A stale store path can make a wrong hash look right.
4. **Group 4, security code.** Audit items 2 and 11:
   - first, write down each caller's current policy under Deviations or
     Notes: notify `GuardedClient`, draft `GuardedClient`, `listModels`,
     `runHTTPTest`. Then route each through
     `source.HTTPClient(cfg.ServiceEndpoint(u)|PrivateEndpoint, …)`, keeping
     its URL and scheme checks;
   - `config.Loopback(host)` replaces the 5 loopback checks: `localhost`,
     or an IP with `IsLoopback()`.

   Verify: the full suite, especially the netguard, notify, draft, models,
   connection-test and security tests, unchanged.
   Traps: link-local addresses (169.254/16, metadata) must stay blocked
   wherever they are blocked today.

## Tests

```sh
go vet ./... && go test -race ./...
nix build --rebuild .#siphon.goModules   # after step 3
git diff --stat main                     # roughly -400 lines
```

## Rollback

Revert the PR; there is no behaviour or data change.

## Deviations

None yet.

## Audit findings (verbatim, ponytail-audit of main at 675db3a)

Audit done. I changed no files. Most of what can be cut is duplicated code. Only three functions are actually dead (`deadcode` found 3, and `staticcheck -checks U1000` only flagged `emptyIfNil`). No dependency can be dropped.

1. `shrink:` 5 copies of a sorted-keys helper (`config.sortedKeys`, `web.sortedKeys` and `web.sortedKeysOf` in the same package, `job.sortedSources`, `web.sortedKinds`), plus 14 inline "make, range, append, sort.Strings" loops. Replace each with `for _, n := range slices.Sorted(maps.Keys(m))`. ~-95 lines. [internal/config/config.go:1918, internal/web/editors.go:244, internal/web/servicespage.go:185, internal/job/pipeline.go:669, internal/web/configweb.go:202, cmd/siphon/main.go:315, cmd/siphon/main.go:580, internal/action/runner_codex.go:37, internal/agentloop/loop.go:132, internal/config/explain.go:210, internal/web/pages.go:257, internal/web/metrics.go:53, …]
2. `shrink:` 4 hand-copied "resolve, then dial only the checked IPs" HTTP clients (`notify.GuardedClient`, `draft.GuardedClient`, `listModels`, `runHTTPTest`). `source.HTTPClient(cfg.ServiceEndpoint(url) /*or PrivateEndpoint*/, timeout, max)` already does the same Public / PrivateNoLinkLocal dial check, and `connection_test_run.go:269` already uses it this way. Keep the URL and scheme checks and drop the dial code. ~-85 lines. [internal/notify/client.go:26, internal/draft/draft.go:333, internal/web/models.go:134, internal/web/services.go:195]
3. `shrink:` `action.decodeJSON` and `action.convertNumbers` are a byte-for-byte copy of `source.DecodeJSON`, and `action` already imports `source`. Call `source.DecodeJSON`. ~-43 lines. [internal/action/agent.go:218]
4. `shrink:` 9 helpers that turn a nil slice into an empty one, while the generic `emptyIfNil` that would cover them is unused. The copies are `nonNil`, `nonNilRows` and `nonNilEntries` (cmd), `nonNilList` and `nonNilStrings` (web), `catalog.nonNil`, `draft.nonNil` and `config.nonNilStrings`. Keep one generic, e.g. `config.NonNil[T]`. ~-48 lines. [cmd/siphon/describe.go:102, cmd/siphon/verbs.go:178, cmd/siphon/verbs.go:833, cmd/siphon/guided.go:346, internal/web/inventory.go:95, internal/catalog/catalog.go:238, internal/draft/draft.go:118, internal/config/explain.go:259]
5. `native:` `source.Limiter` is a hand-written token bucket. `golang.org/x/time/rate` is already in the module graph through go-sdk, so this adds no new module: `rate.NewLimiter(rate.Limit(perSec), burst).AllowN(now, 1)` still takes the clock as an argument. ~-25 lines. [internal/source/webhook.go:56]
6. `stdlib:` since Go 1.24, `crypto/rand.Read` never returns an error, so the `if _, err := rand.Read…` branches are dead. `cmd/siphon/draft.go:59` and `cmd/siphon/tasks.go:169` also re-implement `catalog.NewSecret` inline; call it instead. ~-24 lines. [internal/action/bridge.go:126, internal/catalog/render.go:106, internal/web/oidc.go:74, internal/job/approval.go:23, internal/action/template.go:146, internal/egress/proxy.go:148, internal/web/forms.go:222]
7. `yagni:` 9 wrappers that only pass the call through: `usageErr`→`client.Usage`, `parseApply`/`applyItem` alias→`applyfile`, `itoa`, `lower`, `bytesReader`, `job.needsApproval`→`cfg.NeedsApproval`, `providerOf`→`agentProvider`, `asClientErr`→`errors.As`. Call the target directly. ~-21 lines. [cmd/siphon/verbs.go:30, cmd/siphon/verbs.go:432, cmd/siphon/verbs.go:713, internal/web/data.go:16, internal/web/diagapi.go:362, internal/catalog/render.go:22, internal/job/pipeline.go:340, internal/web/data.go:213]
8. `stdlib:` `orDefault`, `firstNonEmpty` and `client.firstOf` are `cmp.Or`. ~-18 lines. [internal/web/connection_test_run.go:234, internal/web/draftapi.go:113, internal/client/client.go:180]
9. `delete:` 3 dead functions. `emptyIfNil` has no callers at all. `writeSecret` and `OrGET` are only called from tests; point those tests at `stageSecret`+`publishSecret` and inline `cmp.Or(m, "GET")`. ~-21 lines. [cmd/siphon/describe.go:102, internal/web/forms.go:184, internal/catalog/catalog.go:278]
10. `shrink:` 4 string-clip helpers (`store.clip`, `web.clip`, `mcpoauth.clip`, `source.clipRunes`). Export `source.clipRunes` and use it everywhere. ~-15 lines. [internal/store/diag.go:16, internal/web/draftapi.go:34, internal/mcpoauth/mcpoauth.go:171, internal/source/mcp.go:318]
11. `shrink:` 5 copies of the "localhost or a loopback IP" check (`servicenow.isLoopback`, `client.isLoopback`, `catalog.loopback`, and two inside `config.loopbackListen`/`cleartextRemote`). Keep one `config.Loopback(host)` and keep the checks themselves. ~-15 lines. [internal/servicenow/client.go:87, internal/client/client.go:213, internal/catalog/render.go:98, internal/config/config.go:1060, internal/config/config.go:1077]
12. `shrink:` the config kind list is defined 3 times (`config.Kinds` map, `applyfile.configKinds`, `cmd configKinds`), and `sortedKinds` exists only to put the map in order. Keep one ordered exported slice and use `slices.Contains`. ~-12 lines. [internal/config/overlay.go:42, internal/applyfile/applyfile.go:14, cmd/siphon/verbs.go:26, internal/web/configweb.go:202]
13. `shrink:` 3 RoundTrippers that just set a header (two `bearerTransport`, one `headerTransport`). Keep one `headerTransport{base, map}`. ~-10 lines. [internal/action/bridge.go:371, internal/source/mcp.go:230, internal/agentloop/loop.go:407]
14. `shrink:` `job.stepNeedsApproval` repeats `config.StepNeedsApproval` except for how it looks up the agent. Change the config version to take the resolved `*Agent` and use it for both. ~-8 lines. [internal/job/routine.go:49, internal/config/config.go:622]
15. `shrink:` `web.applyItem` copies `applyfile.Item` field by field and adds only `Rev`; embed `applyfile.Item` and `toApplyItems` goes away. ~-7 lines. [internal/web/configapi.go:290, internal/web/draftapi.go:299]

net: -447 lines, -0 deps possible.

- **Deps:** they all earn their place. The AWS SDK handles SSO and profiles, go-jose is already a dependency of go-oidc, and goldmark, cron and expr each do real work.
- **Kept on purpose:** the local `jobs ls`/`approve -config` path is used by the README and `ci/container-test.sh`. The hand-written CONNECT dialer in `agentloop/dial.go` is needed because the egress proxy only speaks CONNECT. I left secret masking and CSP code alone.

I did not check config fields read only by Nix or the templates (only the Go side was grepped), or the 6 HTML templates. Nothing was run after the audit because nothing was edited. Items 2 and 11 move network and loopback guard code into shared helpers, so each needs the existing security tests (`TestTemplatesAreCSPClean`, netguard, security_test) to pass before it merges.