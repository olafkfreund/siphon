---
status: draft
issue: 10
spec: spec/2026-10-06-10-review-followups.md
---

# Plan: Fix Copilot review findings still open on main

## Approved decisions (self-contained)

The spec's table is binding. In short, one fix and one test per item:

1. **Strict compare-and-swap in `cred.Store.Save`.** Write only if the stored bytes equal `old` (an absent file counts as nil). No expiry tie-break. The caller logs `credential_writeback_stale` when nothing was written because the bytes didn't match.
2. **`cred.SameAccount`:** two empty JWT subjects mean *different* accounts. Add a comment and README text: the check reads sandbox-controlled fields and stops naive swaps only.
3. **`Pipeline.Requeue`:** a `StopOrphans` error is returned, so `serve` and `run-once` refuse to start.
4. **`agentExec`:** if the agent-result transaction didn't commit (`err` from `handleEvent`), fail the job with "agent result not recorded: …". Rule errors only warn.
5. **Snapshot agent definitions** (`Agents map[string]config.Agent`) in the job payload for agent actions and routine agent steps. `agentExec`/`stepNeedsApproval` prefer the snapshot and fall back to live config for old payloads. MCP sources stay live.
6. **Integer-preserving decode** for MCP `StructuredContent` (marshal, then `source.DecodeJSON`) and for agent `Raw` (`UseNumber` + int64/float64, as a local helper in `internal/action`).
7. **`validate`:** reject userinfo in `http`/`mcp` source URLs.
8. **`config.Parse`:** reject a second YAML document.
9. **`TimeoutSec = ceil(d/1s)`,** minimum 1.
10. **Mask before capping:** mask with all secrets, including write-back values, then apply the final 64 KiB / 1 MiB caps.
11. **agy warning:** "tool allowlist not enforced" whenever the agent has `mcp` servers.
12. **`cred.Store.Put`:** write the new file first, then remove the other variant.
13. **Webhooks.** `validate`: `sha256` preset with `signature_header == timestamp_header` (case-insensitive) is an error. Webhook events also drop `proxy-authenticate` and the headers named in `Connection`.
14. **`rules test` envelope:** `{"headers": {...}, "event": {...}}`, else the whole file is the event. Header keys lower-cased.
15. **Text:** fix the `internal/job/approval.go` token-logging comment and the stale plan-#1 log line.

## Steps

Each step is one commit; cite "Plan step N". Run `go test -race ./...` and `devenv test` after each.

| # | Items | Who | Lane |
|---|---|---|---|
| 1 | 6, 9, 10, 13 (webhook header drop) | Codex | `internal/action`, `internal/source` |
| 2 | 1, 2, 12 | coder | `internal/cred` (+ README for item 2) |
| 3 | 7, 8, 11, 13 (validate), 14, README migration notes | coder | `internal/config`, `cmd/agentgw` (`rules test` only), README |
| 4 | 3, 4, 5, 15, plus the item 1 caller log | Opus | `internal/job`, plan text |
| 5 | Integration, CI green, PR | Opus | none |

Steps 1–3 run in parallel; step 4 follows step 2 (the `Save` semantics).

1. **Codex.**
   - Per-item tests from the spec table: ID `9007199254740993` survives MCP `structuredContent` and agent `Raw`; 500 ms → 1 and 1.2 s → 2; a token at byte 65530 is fully masked; a `Connection: x-secret` request drops `x-secret` and `proxy-authenticate`.
   - **Trap:** `internal/action` must not import `internal/source` (copy the small helper).
2. **coder.**
   - Tests: re-import wins over a stale write-back; empty/empty subjects are refused; a simulated write failure in `Put` keeps the old variant.
   - **Trap:** `Save`'s return is already `(wrote, err)`. Keep the signature.
3. **coder.**
   - Tests: a userinfo URL errors; a second YAML document errors; the agy mcp warning; the sha256 same-header error; a `rules test` envelope with headers fires a header rule.
   - Add a README migration note listing the three new `validate` errors.
4. **Opus.**
   - Tests: a stubbed `StopOrphans` error makes `Serve` and `RunOnce` return an error with nothing requeued; a cancelled context at the agent-result commit makes the job `failed`; a paused routine keeps the old agent prompt after a config edit; a stale write-back logs `credential_writeback_stale`.
5. **Opus.** Merge the lanes; get the full suite, `devenv test` and CI (including the VM test) green; open the PR with a who-did-what table.

## Tests

- `go test -race ./...` green; `devenv test` green.
- PR CI green: test, vm and devenv jobs.

## Handoff and review

- Codex: step 1, in the `../MCP-AgentGateway-codex` worktree on branch `fix/10-codex-lane`.
- coder: steps 2–3.
- Opus: steps 4–5.
- These are small, targeted fixes, so there is no separate review round. CI plus the per-item tests are the gate. Any deviation is recorded below.

## Rollback

Revert the merge commit. The three new `validate` errors only reject configs that were already wrong.

## Deviations log
