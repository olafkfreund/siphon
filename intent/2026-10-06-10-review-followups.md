---
status: draft
issue: 10
author: olafkfreund
---

# Intent: Fix Copilot review findings still open on main

## Problem

GitHub Copilot reviewed PRs #2–#8 automatically, and nobody read those
reviews before merging. It left 26 comments. The later review rounds fixed
some of them; the findings below **still apply to `main` at c8b1ec4**. They
were checked against the code on 2026-10-06 ("confirmed"), or are taken from
the review text and still need confirming in the spec ("to confirm").

### Medium: correctness and credential safety

1. **A re-import can be overwritten by an older run.** `cred.Store.Save`
   (`internal/cred/cred.go` ~305), *to confirm*.
   - Scenario: a job starts with login A, the operator re-imports login B,
     then the old job's write-back runs.
   - The compare-and-swap fails, because the store no longer holds A. The
     "keep the later expiry" fallback then lets A's refreshed token overwrite
     B.
   - With one run per login by default, a mismatch only ever means a
     re-import or another process, so a strict compare-and-swap (never
     overwrite on mismatch) is both safer and simpler.
2. **The write-back account check trusts a field the sandbox writes.**
   `cred.SameAccount` (~158), *to confirm*.
   - An agent can keep `account_id` and swap the tokens underneath it, and
     the fallback accepts two missing subjects.
   - Real identity would need the provider's API. At minimum the limitation
     must be documented, and two missing subjects must not count as the same
     account.
3. **A failed orphan stop still lets jobs requeue.** `internal/job/queue.go`
   (Requeue), *confirmed*.
   - If stopping leftover `agentgw-action@` units errors or times out,
     agentgw only logs it and requeues their jobs, so a step can run twice at
     once.
4. **Agent results can be silently lost.** `internal/job/pipeline.go`
   (agentExec), *to confirm*.
   - Every error from the `agent-result` `HandleEvent` is treated as a rule
     error and the job is marked `done`, even when the transaction never
     committed (a cancelled context or a database error). That result is
     gone.
5. **Paused routines don't snapshot their agent definitions.**
   `internal/job/routine.go` (~263), *to confirm*.
   - The steps are snapshotted, but the agent definition, its MCP sources and
     approval are read from the live config when the routine resumes.
   - Partly a design choice; it needs a decision.

### Low: precision, config edge cases, hygiene

6. **Large numbers lose precision.** *Confirmed.*
   - MCP `structuredContent` is passed through as go-sdk decoded it, as
     float64 (`internal/source/mcp.go:76`).
   - Agent `Raw` uses plain `json.Unmarshal` (`internal/action/agent.go:177`).
   - IDs above 2^53 change or collide in rules and dedup.
7. **Credentials in URLs aren't rejected.** `https://user:pass@host` source
   URLs pass validation and bypass the `env:`/`file:` rule. *Confirmed.*
8. **Multi-document YAML is half-ignored.** Only the first document of a
   config file is decoded; anything after `---` is silently ignored.
   *Confirmed.*
9. **Sub-second timeouts round down to zero.** `int(timeout / time.Second)`
   turns `500ms` into 0, which disables the timeout inside the unit.
   *Confirmed* (`internal/action/cmd.go:83`).
10. **Masking misses a token cut by the output cap.** A refreshed token that
    straddles the 64 KiB cap is masked only after capping, so its prefix can
    persist. *To confirm.*
11. **agy agents with MCP servers and no `allowed_tools` get no warning,**
    although every tool on those servers is exposed. *To confirm.*
12. **Switching import variants isn't atomic.** Switching a Claude credential
    between a login file and a setup-token deletes the old file before the
    new one is written. *To confirm.*
13. **Webhook config and header edge cases.** *To confirm.*
    - The `sha256` preset accepts the same header for the signature and the
      timestamp, so such a source can never authenticate.
    - Header names nominated by `Connection`, and `Proxy-Authenticate`, aren't
      dropped from events.
14. **`rules test` can't take headers,** so rules using `headers` can't be
    dry-run. *To confirm.*
15. **Stale text.** A comment in `internal/job/approval.go` and one plan-log
    entry still describe logging the approval token, which the code
    deliberately doesn't do. *To confirm.*

## Proposed outcome

- Every item above is either fixed with a test, or recorded with a reason as
  a deliberate decision.
- The two credential items (1, 2) close the remaining write-back gaps that
  are possible without calling provider APIs.
- `main` stays green in the new CI (#9).

## Affected users and systems

`internal/cred`, `internal/job`, `internal/action`, `internal/source`,
`internal/config`, `cmd/agentgw`, README and plan text. No host changes.

## Constraints

- Smallest correct fix for each item. No new features. No behaviour change
  beyond what each item needs.
- Credential handling must never get weaker, and every credential fix needs a
  test.
- Keep compatibility with existing configs, except where a config is wrong
  (credentials in URLs, multi-document files, identical signature and
  timestamp headers). Those become `validate` errors, which the docs note.

## Open questions

1. **Item 2:** document the limit plus the stricter fallback, or also add an
   optional live identity check per provider (an extra API call per
   write-back)? Default: document plus the stricter fallback.
2. **Item 5:** snapshot the agent definition too (consistent with the step
   snapshot), or keep resolving it live, so a fixed agent config applies to
   paused routines? Default: snapshot, for consistency and safety.
3. **Item 3:** when stopping orphans fails, should startup refuse to start, or
   start but leave those jobs `running` until a later stop succeeds? Default:
   refuse to start (fail fast; systemd restarts and retries).
