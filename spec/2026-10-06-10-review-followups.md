---
status: draft
issue: 10
intent: intent/2026-10-06-10-review-followups.md
---

# Spec: Fix Copilot review findings still open on main

All 15 intent items were confirmed in the code on 2026-10-06 (main at
`abb842d`). Each has one fix and one test. The approved resolutions apply:
item 2 is documented plus a stricter fallback; item 5 snapshots agent
definitions; item 3 refuses to start.

## Design

| # | Where | Fix | Test |
|---|---|---|---|
| 1 | `cred.Store.Save` | **Strict compare-and-swap.** Write only if the stored bytes equal `old` (or the file is absent and `old` is nil). Drop the expiry tie-break entirely. A mismatch returns `wrote=false` and is logged by the caller as `credential_writeback_stale`. | Re-import B while a run started from A writes back A′: the store keeps B. |
| 2 | `cred.SameAccount` + README | Codex: if both `account_id` values are empty, compare JWT `sub`, and **two empty subjects count as different** (refuse). Comment and README: the check reads fields the sandbox writes and stops accidental or naive swaps, not a determined attacker; the real mitigations are a dedicated account and egress limits (already in the README). | Empty/empty is refused; same id is accepted. |
| 3 | `job.Pipeline.Requeue` | If `action.StopOrphans` returns an error (including the 30 s timeout), **return the error**, so `serve` and `run-once` refuse to start. systemd's `Restart=on-failure` retries. Log what to check. | With a stubbed StopOrphans error, `Serve` returns an error and no job is requeued. |
| 4 | `job.agentExec` | Split "rules evaluated with errors" from "nothing committed". `handleEvent` already returns `(…, ruleErr, err)`; use it. If `err != nil` (nothing committed), the job **fails** with "agent result not recorded: …". Only `ruleErr` is warned. | A cancelled context during the agent-result commit makes the job `failed`, not `done`. |
| 5 | `job` enqueue + routine | At enqueue, the payload also snapshots `Agents map[string]config.Agent` for the agent action or for every agent step of the routine. `agentExec` and `stepNeedsApproval` use the snapshot when present and fall back to live config for old payloads. MCP source definitions keep resolving live, because they hold resolved secrets that must not be persisted. | Edit an agent's prompt while its routine is paused: the resumed step uses the old prompt. |
| 6 | `source/mcp.go`, `action/agent.go` | Re-decode with integer preservation. MCP: marshal `StructuredContent`, then `DecodeJSON`. Agent `Raw`: `json.Decoder.UseNumber` plus the same int64/float64 conversion. The helper is copied into action as `decodeJSON`, because action must not import source. | ID `9007199254740993` survives both paths unchanged. |
| 7 | `config` | Reject URLs with userinfo (`url.Parse(...).User != nil`) for `http` and `mcp` sources, with an error pointing to `auth.bearer`/`headers`. | `https://u:p@h` gives a validate error. |
| 8 | `config.Parse` | After the first `Decode`, a second `Decode` must return `io.EOF`; otherwise error "multiple YAML documents are not supported". | A `---` second document gives a parse error. |
| 9 | `action/cmd.go` | `TimeoutSec = ceil(timeout / 1s)`, minimum 1. | 500 ms gives 1 s, 1.2 s gives 2 s. |
| 10 | `action/agent.go` | Mask before capping: `runCommand` takes the full secret list including write-back values. The template path already reads the files with a 1 MiB cap plus slack, so mask after reading the write-backs, **then** apply the final caps (64 KiB output / 1 MiB stdout). | A refreshed token at byte 65530 is fully masked. |
| 11 | `config.Warnings` | agy warning "tool allowlist not enforced" when the agent has `mcp` servers, whether or not `allowed_tools` is set. | An agy agent with mcp and no allowed_tools gets the warning. |
| 12 | `cred.Store.Put` | Write the new file first (atomic), **then** remove the other variant. | Simulate a write failure: the old variant survives. |
| 13 | `config` + `source/webhook.go` | `validate`: the `sha256` preset errors when `signature_header` equals `timestamp_header` (case-insensitive). Webhook: also drop `proxy-authenticate` and every header named in the request's `Connection` value. | Both cases. |
| 14 | `cmd/agentgw` `rules test` | Accept an event envelope: if the JSON has top-level `"headers"` and `"event"`, use them; otherwise the whole file is the event (backwards compatible). Header keys are lower-cased like webhooks. | A rule on `headers["x-github-event"]` fires in a dry run. |
| 15 | `job/approval.go`, plan #1 | Fix the comment ("callers must never log the token") and correct the stale plan-log line. | none (text) |

## Alternatives rejected

- **A live provider identity check per write-back (item 2):** an extra network
  call and provider-specific APIs; the owner chose to document instead.
- **Keeping the expiry tie-break (item 1):** it is exactly what lets a stale
  run override a deliberate re-import.
- **Snapshotting MCP source definitions too (item 5):** they contain resolved
  bearer tokens, which must not be written into the job database.
- **Starting with orphan-affected jobs held instead of refusing to start
  (item 3):** that needs a new job state; refusing to start is simpler, and
  systemd retries.

## Risks

| Risk | Mitigation |
|---|---|
| Strict compare-and-swap drops a legitimate refresh if something else touched the store | With one run per login by default, only a re-import or a second process can, and the operator then wins. Logged as `credential_writeback_stale` |
| Refusing to start on an orphan-stop error causes restart loops when systemd or D-Bus is broken | systemd's restart limits apply, and the logged error names the cause |
| New validation errors (items 7, 8, 13) break existing configs | Only configs that were already wrong. Noted in the README migration notes |
| A payload snapshot adds bytes per job | Agent definitions are small. MCP sources are not snapshotted |

## Verification

- Every test in the table, `go test -race ./...`, and `devenv test`.
- CI (#9) is green on the PR, including the NixOS VM test.
- The README migration notes list the new `validate` errors (items 7, 8, 13).
