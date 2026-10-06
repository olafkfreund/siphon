---
status: draft
issue: 16
intent: intent/2026-10-06-16-rename-siphon.md
---

# Spec: Rename the project to Siphon

## Design

A mechanical, test-gated rename in one PR, with compatibility aliases
until v0.2.0. Every name moves as follows:

| Thing | Now | After | Compatibility |
|---|---|---|---|
| Go module | `github.com/olafkfreund/MCP-AgentGateway` | `github.com/olafkfreund/siphon` | none needed (no external importers) |
| Command dir / binary | `cmd/agentgw`, `agentgw` | `cmd/siphon`, `siphon` | package installs `bin/agentgw → siphon` symlink; when invoked as `agentgw` it prints a one-line deprecation notice to stderr |
| Nix package | `pname = "agentgw"` | `pname = "siphon"`, `meta.mainProgram = "siphon"` | `packages.<sys>.agentgw` alias attr |
| Default config file | `agentgw.yaml` | `siphon.yaml` | if `-config` is not given and `siphon.yaml` is missing, use `agentgw.yaml` with a deprecation warning |
| JSON schema | `schema/agentgw.schema.json` | `schema/siphon.schema.json` | old file kept as a copy until v0.2.0 (editors pointing at it keep working); `TestSchemaUpToDate` checks both |
| NixOS module | `services.agentgw.*` | `services.siphon.*` | `lib.mkRenamedOptionModule [ "services" "agentgw" ] [ "services" "siphon" ]`: old settings apply, with an eval warning |
| System user / groups | `agentgw`, `agentgw-io` | `siphon`, `siphon-io` | migration below |
| Units | `agentgw.service`, `agentgw-action@`, `agentgw-action-open@` | `siphon.service`, `siphon-action@`, `siphon-action-open@` | old units are no longer defined, so two daemons can never run on one DB; at start, `StopOrphans` also stops leftover `agentgw-action*@` instances |
| polkit regex | `^agentgw-action(-open)?@[0-9a-f]{16}\.service$` | `^siphon-action(-open)?@[0-9a-f]{16}\.service$` | the rule for the old names is not kept (orphans are stopped by systemd at the migration restart) |
| Paths | `/var/lib/agentgw`, `/var/lib/agentgw-actions`, `/run/agentgw` | `/var/lib/siphon`, `/var/lib/siphon-actions`, `/run/siphon` | one-time state migration below |
| Session cookie | `agentgw_session` | `siphon_session` | users sign in again once |
| Temp prefixes, log text, error strings | `agentgw-…` | `siphon-…` | none needed |
| Example env | `AGENTGW_TOKEN` | `SIPHON_TOKEN` | examples only; the env names are the operator's choice |
| Repo | `olafkfreund/MCP-AgentGateway` | `olafkfreund/siphon` | GitHub redirects; renamed with `gh repo rename` **only after the owner's go-ahead on the PR** |
| Docs | README, `docs/`, `examples/` | Siphon name, logo header, tagline | past `intent/`, `spec/` and `plan/` files are left as written (history) |

- **State migration on NixOS.** `siphon.service` gets an
  `ExecStartPre=+` script, which runs as root. If `/var/lib/agentgw/state.db`
  exists and `/var/lib/siphon/state.db` doesn't, it:
  - copies `/var/lib/agentgw` to `/var/lib/siphon`, preserving modes (DB,
    `credentials/`, and later the overlay secrets from #15);
  - `chown -R siphon:siphon`;
  - writes a `MIGRATED_FROM_AGENTGW` marker;
  - logs one line.

  The old directory is left in place for the operator to remove, so the
  migration can always be rolled back. `/var/lib/agentgw-actions` is not
  migrated: it holds only per-run scratch.
- **Off NixOS** (local and microVM), nothing moves. The default
  config-file fallback covers the local instance. The dev instance on p620
  is moved by hand to `~/.local/state/siphon-dev`.
- **A guard against stragglers.** A test walks the repo and fails on
  `agentgw` (case-insensitive) outside an allowlist:
  - the alias code;
  - the migration script;
  - the old schema copy;
  - `intent/`, `spec/` and `plan/` history;
  - the README's "Formerly agentgw" note;
  - CHANGELOG lines.

## Alternatives rejected

- **Renaming in stages** (binary first, module later): this leaves the
  project half-named across several PRs, and #15's new code would land
  with the old names.
- **No aliases:** nothing is deployed in production, but the owner's local
  instance and CI scripts use the old names. Aliases cost about 20 lines
  and are removed at v0.2.0.
- **Keeping the old state paths under the new name:** this confuses
  operators reading `/var/lib`. The migration is small and nothing is
  deployed yet (intent Q2).

## Risks

| Risk | Mitigation |
|---|---|
| A missed reference breaks a path at runtime (for example the polkit regex no longer matching the template name) | The repo-wide straggler test, and the full NixOS VM test on the renamed units |
| The migration chowns the wrong tree or runs twice | It only runs when the old DB exists and the new one doesn't; there is a marker file; it copies rather than moves; VM test |
| `mkRenamedOptionModule` on a whole service prefix misbehaves with submodule `settings` | An eval check builds a NixOS system using only `services.agentgw.*` and asserts the warning and the generated unit |
| The Go module path change breaks CI caching | No impact on correctness; one cold cache |
| The repo rename breaks the flake URL people use | GitHub redirects git and https; README shows the new URL |

## Verification

- `go vet ./...` and `go test -race ./...`, including the straggler test and
  the default-config fallback test.
- `devenv test`.
- `nix build .#siphon .#agentgw`. The second is the alias; both provide
  `bin/siphon` and the `bin/agentgw` symlink.
- The NixOS VM test, all subtests, on the renamed module. It gains a
  migration subtest: seed `/var/lib/agentgw/state.db` (with an imported
  login), start `siphon.service`, and assert the job history and the login
  are present and owned by `siphon`.
- A new flake check `checks.<sys>.legacy-option`: evaluate a system using
  `services.agentgw.enable = true` and assert the eval warning and
  `siphon.service` in the result.
- CI green, then the owner approves the repo rename, then
  `gh repo rename siphon`, then the README and flake URLs are confirmed.
