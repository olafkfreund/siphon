---
status: draft
issue: 133
author: olafkfreund
---

# Intent: remove duplicated helpers and dead code

## Problem

A whole-repo over-engineering audit (the ponytail plugin's `ponytail-audit`)
found no unnecessary dependencies, but much duplicated code. Some copies have
already started to drift apart:

- **Small helpers defined many times:**
  - sorted keys (5 copies and 14 inline loops);
  - nil-to-empty (9);
  - string clipping (4);
  - loopback checks (5);
  - header RoundTrippers (3);
  - `cmp.Or` reimplementations (3);
  - the config kind list (3).
- **Copied logic:**
  - 4 hand-copied DNS-guarded HTTP clients, next to the shared
    `source.HTTPClient`;
  - a byte-for-byte copy of `source.DecodeJSON`;
  - a second step-approval check;
  - a field-by-field copy of `applyfile.Item`.
- **A hand-written token bucket** where `golang.org/x/time/rate` is already in
  the module graph.
- **Dead code:**
  - `rand.Read` error branches, which can't fire since Go 1.24;
  - 9 wrappers that only pass the call through;
  - 3 dead functions.

The estimate is about -447 lines.

## Proposed outcome

Each concept has one implementation, used everywhere. Behaviour is unchanged,
and the existing tests pass unchanged. The exceptions are tests of deleted
dead functions, which are pointed at the real code paths.

## Affected users and systems

Contributors only; no behaviour change. The guarded HTTP clients and the
loopback checks are security code, and are the riskiest part.

## Constraints

- No behaviour change, no new direct dependency beyond promoting
  `golang.org/x/time` from indirect.
- Leave alone: secret masking, CSP code, the agentloop CONNECT dialer, and
  the local `jobs ls`/`approve -config` path.
- Security consolidation (the HTTP clients, the loopback check) must keep
  each caller's current policy: public only, or private without link-local,
  and the scheme and URL checks.
- Do it after #131, #132 and the bug-fix issue merge, to avoid conflicts.

## Open questions

None.
