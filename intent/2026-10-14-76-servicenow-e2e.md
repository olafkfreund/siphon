---
status: approved
issue: 76
author: olafkfreund
---

# Intent: ServiceNow end to end in CI, and the official ServiceNow MCP server

## Problem

The ServiceNow integration (#71) works. Inbound signed events (#73), agent
tools through `siphon mcp-servicenow` (#74) and the native `servicenow:`
action (#75) are all merged, and they were checked by hand against the
demo instance `calitiiltddemo3`. Three gaps remain.

1. **Nothing guards the whole chain in CI.** A signed delivery fires a
   rule, the rule runs a `servicenow:` action, and the action calls
   ServiceNow with OAuth. Each part has unit tests against fakes, but
   nothing runs them together in the NixOS VM test, where the real
   sandbox, the egress proxy and the bridge apply. A regression in the
   wiring (the bridge env from #72, egress to the instance host, OAuth
   through the proxy) would only show on a real instance.
2. **ServiceNow's own MCP server isn't connectable.**
   - Licensed instances with Now Assist run an official MCP server, the
     "MCP Server Console". The demo instance has it: there's an
     `sn_mcp_server.default` OAuth app, using the authorization-code
     grant.
   - Siphon's `auth.oauth` MCP sources could log in to it, but nobody has
     tried, and it's unknown whether ServiceNow serves the OAuth discovery
     metadata (RFC 9728 / RFC 8414) that the login needs.
   - Users with Now Assist would rather use ServiceNow's maintained tools
     than Siphon's built-in nine.
3. **The docs** don't describe the whole picture in one place: which path
   to use when (built-in tools, native action, official MCP, webhooks).

## Proposed outcome

- **A NixOS VM test case,** using `nix/stub-servicenow.py`, a small fake
  instance like `stub-model.py`, which runs the whole chain:
  - a Standard-Webhooks-signed delivery to a `servicenow-hooks` source;
  - a rule runs a `servicenow: add_work_note` action;
  - the stub gets an OAuth token request, then a `PATCH` with
    `Authorization: Bearer <token>`, through the egress proxy, to an
    allowed private endpoint.
  - It asserts that the work note arrived, and that the token is absent
    from the job output and the journal.
- **The official MCP server as an option:**
  - first, a spike against the demo instance: can `auth.oauth` discover
    and log in to the MCP Server Console?
  - if it works, the catalog `servicenow` entry gains a choice of MCP
    server: built-in (default) or official (remote, `auth.oauth`);
  - if it doesn't, the spike's findings go in the docs, and an issue
    records what ServiceNow is missing.
- **A "ServiceNow: which path when" section** in
  `docs/connections/servicenow.md`, plus `docs/llm.md` and the
  `docs/README.md` links.

## Affected users and systems

- ServiceNow users. CI, through the VM test (`nix flake check`, the GitHub
  VM job).
- `nix/vm-test.nix`, a new `nix/stub-servicenow.py`,
  `internal/catalog/services.yaml` (the official option), and the docs.
  `mcpoauth` only if the spike needs a fix.

## Constraints

- **The VM test stays self-contained:** no network, and no real
  ServiceNow. The stub is local Python, as the existing stubs are.
- **The spike uses the demo instance** and its agenix credentials. It
  changes nothing on the instance beyond an OAuth login, which needs an
  interactive browser consent, so you do the click.
- **No secrets in the test output or the repo.**

## Open questions

- **Polling template.** The roadmap planned a polling template (a `type:
  http` source querying recent incidents). Since then, webhooks (#73) and
  the native action (#75) cover inbound and outbound, `http` sources can't
  do OAuth, and the demo instance has no REST API key. **I'd drop it**
  unless you want a polling fallback.
- **The official-MCP spike needs you once,** to complete the OAuth login
  in a browser against `calitiiltddemo3`. Is that OK?

## Decisions (owner, at approval)

- **The polling template is dropped.** Webhooks and the native action
  cover both directions.
- **The owner will complete the OAuth consent** in a browser for the
  official-MCP spike.
