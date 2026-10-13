---
status: draft
issue: 72
author: olafkfreund
---

# Intent: bridged MCP servers reach their API through the egress proxy

## Problem

A stdio MCP server with secret `env` (or AWS keys) runs in its own
`siphon-mcp@` unit, behind `siphon mcp-bridge`. The unit has no network of
its own (`PrivateNetwork=true`, `nix/module.nix:128`); its only way out is
the per-job egress proxy. The bridge unit is given that proxy:
`bridgeUnit` sets `HTTPS_PROXY` and friends through `egressSetup`
(`internal/action/bridge.go:228-231`).

But `mcpbridge.Run` (`internal/mcpbridge/bridge.go:42-63`) starts the actual
server with a fresh environment: only `PATH`, `HOME`, `LANG` and the
secrets. The proxy variables are dropped, so the server has no route to its
API. A bridged package like `github`, or the planned
`siphon mcp-servicenow` (#74), can list its tools but every call that
reaches out fails.

No test catches it. The VM test checks proxy env in the action sandbox,
not in a bridged server.

## Proposed outcome

- A bridged MCP server gets the proxy variables its unit was given, and
  reaches an allowlisted host through the egress proxy.
- It still can't reach a host outside its allowlist (`BridgeEgress`).
- A VM assertion proves both, and it fails on `main` before the fix.

## Affected users and systems

- Every bridged stdio MCP source: catalog packages with secret env, and
  AWS servers.
- Phase 2 of the ServiceNow integration (#71, #74), which depends on this.
- `nix/vm-test.nix`.

## Constraints

- The proxy URL carries a per-job credential. It may reach the bridged
  server, which needs it, but it must stay masked in output and errors.
  It's already in `o.Secrets` through `egressSetup`'s `extra`.
- The server's environment is otherwise unchanged: no other host
  variables leak in.
- Egress stays limited to the source's allowed hosts.

## Open questions

- Should the bridge pass only the six proxy variables, or should the unit's
  `JobSpec.Env` be passed through as a whole? **I'd pass the proxy
  variables only:** an explicit list, the same as `PATH`, `HOME` and `LANG`.
