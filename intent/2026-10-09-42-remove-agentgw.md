---
status: approved
issue: 42
author: olafkfreund
---

# Intent: remove the legacy agentgw names

## Problem

The rename to Siphon (#16) kept compatibility shims for the old name,
`agentgw`. Each was promised to go away in v0.2.0, and we're at v0.4.0:

- **The binary:** an `agentgw` symlink, which prints "this alias goes away in
  v0.2.0" (`flake.nix:39`, `cmd/siphon/main.go:42`).
- **Nix:**
  - the `.#agentgw` package (`flake.nix:52`);
  - the `services.agentgw` option path (`nix/module.nix:181`);
  - its eval check (`flake.nix:171-211`).
- **Config fallbacks:**
  - `agentgw.yaml` is read when `siphon.yaml` is missing
    (`cmd/siphon/main.go:158`);
  - an existing `agentgw.db` is used when `db:` is unset
    (`internal/config/config.go:513`).
- **State migration:** on NixOS, a one-time copy from `/var/lib/agentgw`
  (`nix/module.nix:162-177`), with a VM subtest (`nix/vm-test.nix:592`).
- **The schema:** a duplicate `schema/agentgw.schema.json`, kept identical
  by a test.
- **Docs:** the README's "Upgrading from agentgw" section and its "Formerly
  agentgw" line.
- **CI:** builds `.#agentgw` and the legacy option check
  (`.github/workflows/ci.yml:25`).

The shims now cost more than they give:
- every feature has to regenerate two schemas;
- CI builds an extra package and an extra check;
- the VM test spends a subtest on a migration nobody needs;
- the binary prints a promise that was broken twice.

Every tagged release (v0.1.0 through v0.4.0) already shipped as `siphon`.
`agentgw` only existed on `main` before the first release.

## Proposed outcome

- **No shims.** None of them remains in the code, Nix, CI or user docs.
  `agentgw` appears only in history: intent, spec, plan and research files,
  and the changelog or release notes.
- **Stricter straggler test.** `TestNoStragglers` loses its `legacy-name`
  escape. Any new "agentgw" outside history fails the build.
- **Clear upgrade note.** The next release notes say what was removed. They
  also give the one manual step left for anyone still on pre-v0.1.0 state:
  rename `agentgw.yaml` and `agentgw.db`, or move `/var/lib/agentgw`.

## Affected users and systems

- **Code:** `cmd/siphon` (main, legacy test), `internal/config` (the db
  fallback, the schema test, its tests) and `internal/naming_test.go`.
- **Nix and CI:**
  - `flake.nix` (the symlink, the package alias, the legacy-option check);
  - `nix/module.nix` (the rename module, the state migration);
  - `nix/vm-test.nix` (the migration subtest);
  - `.github/workflows/ci.yml`.
- **Files:** `schema/agentgw.schema.json` (deleted), `.gitignore`,
  `README.md`.
- **Hosts:** any host still configured with `services.agentgw`, or still
  holding `/var/lib/agentgw`. On a host that already migrated, the old
  directory stays on disk, untouched.

## Constraints

- **Never delete state.** The removal stops reading `/var/lib/agentgw`; it
  never removes it.
- **Fail loudly.** A host that still sets `services.agentgw` should fail
  evaluation with a message that names the new option, not ignore the
  setting. `lib.mkRemovedOptionModule` does this for free.
- **No change for current users.** Anyone on `siphon` names sees no
  difference.
- **Production hosts:** no change to any of the owner's hosts without asking.
  Before the merge, check whether any of them still uses `services.agentgw`
  or `/var/lib/agentgw`.

## Open questions

1. **Old option path:** fail evaluation with a pointer
   (`mkRemovedOptionModule`), or drop it so Nix reports a generic unknown
   option? Recommendation: `mkRemovedOptionModule`. It is one line, and the
   error says what to do.
2. **Keep the one-time `/var/lib/agentgw` migration** for a few more
   releases? Recommendation: remove it. No tagged release used that path,
   and the release notes give the one `mv` command.
3. **Version:** ship this in v0.5.0 together with #40 (the scrape token)?
   Recommendation: yes. Removing the shims breaks compatibility for
   pre-release installs, so it shouldn't be a patch release.

## Resolved (owner approval, 2026-10-09)

All three recommendations accepted:
1. `services.agentgw` uses `mkRemovedOptionModule`, with a pointer to `services.siphon`.
2. The `/var/lib/agentgw` migration is removed. The release notes give the `mv` command.
3. This ships in v0.5.0, together with #40.
