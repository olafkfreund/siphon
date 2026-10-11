---
status: draft
issue: 54
author: olafkfreund
---

# Intent: OAuth MCP sources keep working for the whole of an agent run

## Problem

**What happens today:**
- **The token:** an agent run that uses an `auth.oauth` MCP source gets
  that source's access token once, when the run starts. It's a static
  `Authorization: Bearer` header in the agent's MCP config
  (`internal/job/pipeline.go`, `agentExec`).
- **The mismatch:** access tokens often last an hour or less, while an
  agent's `timeout` can be longer.
- **The failure:** once the token expires, every call to that server
  fails with 401 for the rest of the run. The agent can't refresh it. By
  design it never gets the refresh token.
- **The exposure:** the agent holds a live access token for the whole
  run. It's masked in job output, but the agent process and anything it
  runs can read it and use it, until the token expires.

## Proposed outcome

- **Long runs:** an agent run that outlasts the access token keeps
  working against its OAuth MCP sources. Siphon refreshes the token as
  needed, exactly as it does for polling.
- **No token for the agent:** the agent never sees the OAuth access token
  or the refresh token. It gets a loopback URL and a per-run key, the way
  bridged stdio servers already work (`internal/action/bridge.go`).
- **Nothing changes for users:** no new config, and the same tool
  allowlists.
- **Failures read the same:** a revoked login fails the call with the
  usual "OAuth login required" message.

## Affected users and systems

- `internal/job/pipeline.go` (`agentExec`), `internal/action` (the agent
  bridges and forwards), and `internal/mcpoauth` (tokens).
- Agent runs that list an `auth.oauth` source in `mcp:`, in every sandbox
  mode (systemd units and `none`).
- **Egress:** the agent no longer connects to the OAuth MCP server
  itself; the daemon does. So the agent's egress allowlist no longer
  needs that host. The daemon's connection uses the source's existing
  client: the private-address guard, `allow_private`, and no redirects.
- **The VM test:** an agent run with an OAuth source, if the harness
  allows it.
- No config, schema or migration. No hosts.

## Constraints

- **Must:**
  - keep Streamable HTTP working end to end, including server-sent
    events (streamed responses, no buffering);
  - authenticate the agent to the loopback endpoint with a per-run
    secret, so other local processes can't use it;
  - stop the endpoint when the run ends;
  - mask the per-run secret;
  - forward only to the source's own `url`, never to a host the agent
    chooses.
- **Must not:**
  - give the agent the access or refresh token;
  - add a dependency;
  - weaken the sandbox, for example by opening the unit to the daemon's
    TCP ports. Reuse the existing unix-socket forwards.

## Open questions

1. **The approach:**
   - **(a)** A daemon-side proxy, as above. It fixes expiry and keeps the
     token from the agent. My proposal.
   - **(b)** Refresh before the run only when the token would expire
     within the agent's `timeout`. It's tiny, but can't help when the
     server's token lifetime is shorter than the run, and the agent still
     holds the token.
2. **Tool allowlists:** a bridged stdio server enforces the agent's
   `allowed_tools` in the bridge too (`bridgeTools`). Should the OAuth
   proxy also refuse `tools/call` for tools that aren't allowed?
   - **(a)** No. It passes MCP through unchanged, and the agent CLI's
     `--allowedTools` stays the enforcement, as it is today for remote
     servers. My proposal: parsing JSON-RPC in a streaming proxy is
     complexity for a check that already exists.
   - **(b)** Yes, defence in depth.
