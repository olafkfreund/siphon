---
status: approved
issue: 16
author: olafkfreund
---

# Intent: Rename the project to Siphon

## Problem

The project is called `agentgw` (repo `MCP-AgentGateway`). The name is
descriptive but forgettable, and it has no identity to build a portal,
logo or README around.

The owner chose **Siphon**: an octopus draws water in and jets it out
through its siphon, just as this project draws events in and jets agents
out. The brand (logo, colours, visual language) is defined in #15
(`docs/brand/`).

The name `agentgw` is spread through every layer:
- the binary and CLI;
- the Go module path;
- the NixOS module (`services.agentgw`);
- the system user and groups (`agentgw`, `agentgw-io`);
- the systemd units (`agentgw.service`, `agentgw-action@`,
  `agentgw-action-open@`);
- the state paths (`/var/lib/agentgw`, `/var/lib/agentgw-actions`,
  `/run/agentgw`);
- the config file name (`agentgw.yaml`) and the JSON schema;
- the session cookie, temp-file prefixes and the README.

There are about 300 references in all.

## Proposed outcome

1. **Everything user-facing says Siphon.** The CLI is `siphon`, with
   `siphon serve`, `siphon validate`, and so on. Also:
   - the NixOS module is `services.siphon`;
   - the systemd units are `siphon.service`, `siphon-action@` and
     `siphon-action-open@`;
   - the config file is `siphon.yaml`, with the schema
     `siphon.schema.json`;
   - the portal, README and docs carry the Siphon logo and name.
2. **The repository is renamed** to `olafkfreund/siphon`. GitHub redirects
   the old URL. The Go module path changes to match.
3. **Existing setups keep working, with a clear nudge to move:**
   - `services.agentgw.*` stays as a renamed-option alias, with a
     deprecation warning at eval;
   - the `agentgw` binary name stays as a symlink for one release;
   - `agentgw.yaml` is still found if `siphon.yaml` isn't there;
   - the state in `/var/lib/agentgw` is migrated (or reused) with no data
     loss: the DB, imported logins and the overlay/revisions from #15.
4. **One change, fully green.** Unit tests, `devenv test`, the NixOS VM
   test (also covering the old option names), and CI all pass after the
   rename.

## Affected users and systems

- **The repo:** every package, `flake.nix`, `nix/`, the CI workflow,
  `devenv.nix`, docs, the schema, examples and the intent/spec/plan
  history. Past artifacts are left as written; only living docs change.
- **The local test instance on p620** (`~/.local/state/agentgw-dev`): to
  be moved to `~/.local/state/siphon-dev`.
- **GitHub:** the repo rename (a setting the owner confirms), and the
  issue and PR links, which redirect.
- No production hosts run agentgw yet, so there is no live migration
  outside the repo.

## Constraints

- **Ordering with #15.** The portal work is mid-spec. Doing the rename
  first, then implementing #15 on top, avoids renaming new code twice.
  Proposal: #16 first, as a mechanical, test-gated change.
- **Security is unchanged.** The renamed units, polkit rule, user and
  groups must keep the exact hardening, including the polkit regex now
  covering `siphon-action(-open)?@`. The VM test proves it.
- **Never two daemons on one DB.** The alias path must not let
  `agentgw.service` and `siphon.service` both run.
- **The repo rename is outward-facing:** it happens only after the owner's
  explicit go-ahead at the PR.

## Open questions

1. **Repo name:** `siphon`, `siphon-gateway` or `siphon-agents`? A bare
   `siphon` is the cleanest, but GitHub search shows other unrelated
   "siphon" repos. Proposal: `olafkfreund/siphon`.
2. **State paths:** move `/var/lib/agentgw` to `/var/lib/siphon` (a one-time
   migration at service start), or keep the old paths under the new name?
   Proposal: migrate. It is cleaner, and nothing is deployed yet.
3. **How long the aliases live:** one release (until v0.2.0) and then
   remove them? Proposal: yes.
4. **Order:** this before #15's implementation (proposal), or after?

## Decisions at approval (2026-10-06)

The owner approved the proposals: repo `olafkfreund/siphon`; migrate state
to `/var/lib/siphon`; aliases until v0.2.0; this rename lands before #15's
implementation.
