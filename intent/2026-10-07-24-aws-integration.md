---
status: draft
issue: 24
author: olafkfreund
---

# Intent: AWS integration with short-lived credentials

## Problem

#23 made GitHub and GitLab easy to connect. AWS was split out, because it
needs more than a token:

- **Credentials.**
  - AWS MCP servers are local programs that need AWS credentials.
  - #23 lets a stdio MCP server receive secrets, but the obvious secret,
    a long-lived access key pair, is the wrong thing to hand to software
    an agent drives. It never expires, and it often carries far more
    rights than the task needs.
  - Siphon has no notion of short-lived credentials: no role assumed for
    a run, no expiry, no session policy.
- **Scope.**
  - Nothing helps the owner give an agent *read-only* AWS access.
  - Nothing limits it to one account, one region, or the services a task
    actually needs.
  - A mistake here can spend money or delete production resources.
- **Network.**
  - AWS has many endpoints (one per service and region, plus STS and SSO).
  - The egress allowlist must cover what the MCP server needs, and nothing
    else. Today the owner would have to work those hosts out by hand.
- **Events.**
  - EventBridge can call Siphon through an API destination, which sends
    an API-key header that #23's `token` webhook mode can check.
  - But nothing tells the owner how to set that up, or shows the URL and
    header to paste.
- **Packaging.**
  - AWS's MCP servers (the `awslabs` family) are Python programs.
  - They are not in nixpkgs, so #23's Nix-pinned package allowlist has
    nothing to point at.

## Proposed outcome

1. **Short-lived credentials per run.** An AWS-backed MCP source never
   holds a long-lived key at run time. Siphon gives each agent run's MCP
   bridge credentials that expire, from one of:
   - a role assumed for the run (optionally with a session policy that
     narrows it further);
   - an existing profile or `credential_process` the owner already uses
     (for example IAM Identity Center/SSO).

   The base secret (if any) stays with Siphon, as in #23. The MCP server and
   the agent only ever see the temporary credentials, and those expire
   soon after the run.
2. **Read-only by default.**
   - Siphon ships a read-only IAM policy template, with the trust policy for
     the role to assume.
   - The docs recommend a dedicated role per use, in a non-production
     account first.
   - Write access is something the owner adds deliberately, never a
     preset.
3. **Egress limited to what is needed.** The bridge's egress allowlist is
   derived from the chosen region and services (plus STS), not from a
   wildcard like `*.amazonaws.com`.
4. **EventBridge events in.** The docs and the Services page show how to
   point an EventBridge API destination at a Siphon webhook, using #23's
   `token` mode:
   - Siphon generates the key and shows it once;
   - the URL and header name are given.
5. **An AWS tile on the Services page.** Like GitHub and GitLab, it covers:
   - a region;
   - the role to assume, or the profile to use;
   - the AWS MCP server;
   - an optional EventBridge webhook;
   - a **Test** button: credentials can be obtained, the identity is the
     expected role, and the MCP server lists its tools.

   Saving creates ordinary, editable config items.
6. **Tested and documented.**
   - Unit tests cover credential handling: expiry, redaction, never in argv
     or logs.
   - A VM test proves the agent never sees the base secret and the bridge
     only gets temporary credentials. AWS STS is faked locally.
   - The README gains an AWS section.

## Affected users and systems

- The owner, and anyone running Siphon against an AWS account.
- Siphon:
  - config: a source credential kind for AWS;
  - the MCP bridge from #23 (credential injection, egress);
  - the Services page;
  - the NixOS module: packaging an AWS MCP server for the
    `mcpPackages` allowlist;
  - the docs.
- AWS: an IAM role and policy in the owner's account. The owner creates
  them; Siphon never does.

## Constraints

- **No long-lived AWS keys reach the agent or the MCP server.** At most,
  Siphon holds a base credential (preferably none, using a profile or
  SSO), stored write-only like every other secret.
- **No live AWS calls in CI.** Tests use a local fake of STS and of the MCP
  server.
- **Siphon never creates or changes IAM resources itself.** It only
  documents and templates them. The owner applies them.
- **#23's rules still hold:**
  - secrets only by reference;
  - the bridge runs in its own sandboxed unit;
  - the agent sees a per-run bearer token, never the secret;
  - restricted network, egress through the proxy only;
  - stdio commands stay file-only in the portal.
- **Pinned packaging.** Any AWS MCP server Siphon offers is built and pinned
  in the flake (no `uvx`/`pip install` at run time).
- **The owner's live AWS checks** (role, Test button, EventBridge) are
  done by the owner, in their own account.

## Open questions

1. **Which AWS MCP server first?** The general `aws-api-mcp-server`
   (calls any AWS CLI command, so its scope depends entirely on IAM), or
   narrow ones (CloudWatch logs and metrics, cost explorer, documentation)
   that are safer by design? Proposal: one narrow server (CloudWatch)
   plus the AWS documentation server, packaged in the flake; the
   general one later if wanted.
2. **Credential source for the first release:** role assumption from a
   base credential that Siphon holds, or only profiles/SSO already on the
   host? Proposal: both, with SSO/profile recommended.
3. **Session length:** default 15 minutes (STS minimum), renewed while a
   run lasts longer, or a fixed run-length session up to 1 hour?
4. **Polling sources:** should an AWS MCP server also be usable as a
   *polling* source (for example, alarms in ALARM state), or only as agent
   tools for now?
