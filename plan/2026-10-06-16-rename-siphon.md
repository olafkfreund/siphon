---
status: draft
issue: 16
spec: spec/2026-10-06-16-rename-siphon.md
---

# Plan: Rename the project to Siphon

## Approved decisions (self-contained)

| Thing | After | Compatibility (until v0.2.0) |
|---|---|---|
| Go module | `github.com/olafkfreund/siphon` | none |
| Binary / cmd dir | `siphon`, `cmd/siphon` | the package installs a `bin/agentgw → siphon` symlink; invoked as `agentgw`, it prints one deprecation line to stderr |
| Nix package | `pname = "siphon"`, `meta.mainProgram = "siphon"` | `packages.<sys>.agentgw` alias |
| Default config | `siphon.yaml` | without `-config`, if `siphon.yaml` is missing, `agentgw.yaml` is used with a warning |
| Schema | `schema/siphon.schema.json` | `schema/agentgw.schema.json` kept as an identical copy; the schema test checks both |
| NixOS module | `services.siphon.*` | `lib.mkRenamedOptionModule [ "services" "agentgw" ] [ "services" "siphon" ]` |
| User / groups | `siphon`, `siphon-io` | the migration chowns |
| Units | `siphon.service`, `siphon-action@`, `siphon-action-open@` | old units are no longer defined |
| polkit | `^siphon-action(-open)?@[0-9a-f]{16}\.service$` | no old rule |
| Paths | `/var/lib/siphon`, `/var/lib/siphon-actions`, `/run/siphon`, `/tmp/siphon` (job files) | state migration (below) |
| Cookie | `siphon_session` | users sign in again |
| Example env | `SIPHON_TOKEN` | none |
| Repo | `olafkfreund/siphon` | GitHub redirect; **only after the owner says go on the PR** |

- **Migration.** `siphon.service` gets `ExecStartPre = "+<script>"`, which
  runs as root. If `/var/lib/agentgw/state.db` exists and
  `/var/lib/siphon/state.db` doesn't, it:
  - runs `cp -a /var/lib/agentgw/. /var/lib/siphon/`;
  - runs `chown -R siphon:siphon /var/lib/siphon`;
  - writes `/var/lib/siphon/MIGRATED_FROM_AGENTGW`;
  - logs one line.

  The old directory is kept. `/var/lib/agentgw-actions` is not migrated.
- **Straggler test.** It fails on `agentgw` (case-insensitive) in tracked
  files outside this allowlist:
  - `intent/`, `spec/`, `plan/`, `research/`;
  - `schema/agentgw.schema.json`;
  - the alias and fallback code lines, each marked `// legacy-name`
    (`# legacy-name` in Nix);
  - the README's one "Formerly agentgw" note.
- **Past artifacts** (`intent/`, `spec/`, `plan/`, `research/`) are left as
  written.

## Steps

Each step is one commit; cite "Plan step N". Run `go vet ./...`,
`go test -race ./...` and `devenv test` after each, and the VM test after
step 2.

| # | Step | Who | Lane |
|---|---|---|---|
| 1 | Go rename: module, cmd dir, strings, fallbacks, schema, straggler test | coder | `go.mod`, `cmd/`, `internal/`, `schema/`, `examples/`, `internal/config/testdata` |
| 2 | Nix rename: flake, module (+ alias, migration, polkit), devenv, VM test (+ migration subtest), legacy-option check | Opus | `flake.nix`, `nix/`, `devenv.nix` |
| 3 | Docs: README (logo header, name, "Formerly agentgw", new URLs), `docs/`, `.gitignore` | coder (after 1) | `README.md`, `docs/adding-a-source.md`, `.gitignore` |
| 4 | Local dev instance moved; PR; repo rename after go-ahead | Opus | none (p620 user dir, GitHub) |

1. **Go** (coder).
   - `go.mod`: `module github.com/olafkfreund/siphon`. Rewrite every import
     (`gofmt -r` or `sed` on the import path), then `go build ./...`.
   - `git mv cmd/agentgw cmd/siphon`.
   - Strings in `internal/action/template.go`:
     - unit names `siphon-action@` and `siphon-action-open@`;
     - `StopOrphans` patterns;
     - `jobFilesDir = "/tmp/siphon"`;
     - comments and messages.
   - Elsewhere:
     - `internal/web` (cookie `siphon_session`, titles, layout and login
       text);
     - the store lock message;
     - the cred hint (`siphon credentials import`);
     - the egress, source and job messages;
     - temp-file prefixes.
   - `cmd/siphon/main.go`: the default `-config` is `siphon.yaml`. If the
     flag wasn't set and `siphon.yaml` doesn't exist but `agentgw.yaml`
     does, use it with the warning
     `agentgw.yaml is deprecated, rename it to siphon.yaml` (`// legacy-name`).
     If `filepath.Base(os.Args[0]) == "agentgw"`, print
     `agentgw is now siphon; this alias goes away in v0.2.0` to stderr.
   - Schema: `git mv schema/agentgw.schema.json schema/siphon.schema.json`.
     The schema `$id` and title become Siphon. Write the legacy copy with
     the same content, and update `TestSchemaUpToDate` to check both files.
   - `git mv examples/agentgw.yaml examples/siphon.yaml` and update its
     `$schema` comment and `SIPHON_TOKEN`. Update `testdata/full.yaml`
     the same way.
   - **Straggler test:** `internal/naming_test.go`, or
     `cmd/siphon/naming_test.go` if a package is needed. It runs
     `git ls-files`, reads each file, and fails on `agentgw` outside the
     allowlist (above).
   - **Tests:** the existing ones, renamed; the config fallback; the argv0
     notice; both schemas checked; the straggler test.
   - **Traps:**
     - unit names in Go must equal `nix/module.nix` (step 2) exactly;
     - the existing tests assert unit names, cookie names and messages,
       so update those assertions, never delete them;
     - `.devenv/` and `result` aren't tracked: use `git ls-files` only.
2. **Nix** (Opus).
   - `flake.nix`:
     - `pname = "siphon"`, `subPackages = [ "cmd/siphon" ]`,
       `meta.mainProgram = "siphon"`;
     - `postInstall` adds `ln -s siphon $out/bin/agentgw` (`# legacy-name`);
     - `packages.siphon` plus `default`, and `packages.agentgw` as an alias;
     - `checks.<sys>.legacy-option`: evaluates `nixpkgs.lib.nixosSystem`
       with `services.agentgw.enable = true` (and `settings.server.token`)
       and asserts that `config.systemd.services ? siphon` and that the
       warnings mention `services.agentgw`.
   - `nix/module.nix`:
     - rename the option root, user, groups, units, paths and the polkit
       regex;
     - `imports = [ (lib.mkRenamedOptionModule [ "services" "agentgw" ] [ "services" "siphon" ]) ]`;
     - the migration as `ExecStartPre=+${pkgs.writeShellScript ...}`, which
       uses absolute coreutils paths;
     - `StateDirectory = "siphon"`;
     - `RuntimeDirectory`/tmpfiles for `/run/siphon` and
       `/var/lib/siphon-actions`.
   - `devenv.nix`: script names and the `agentgw` wrapper become `siphon`.
   - `nix/vm-test.nix`: the renamed service, units and paths everywhere,
     plus a new subtest, "state migration from agentgw":
     1. Stop `siphon.service`.
     2. Move the state to `/var/lib/agentgw` (as if from an old install),
        and remove `/var/lib/siphon/state.db`.
     3. Start the service.
     4. Assert the earlier jobs are listed, the imported logins are still
        there, everything is owned by `siphon`, and the marker exists.
   - **Traps:**
     - The polkit regex and the Go unit names must match. The VM's sandbox
       subtests prove it.
     - The `ExecStartPre=+` script must not run on a fresh install with no
       old dir (guard with a test).
     - `RestrictSUIDSGID` doesn't affect `cp -a` as root, but the copied
       setgid dirs keep their mode.
     - nixfmt reflow: the edits must assert their match (a lesson from
       #7).
3. **Docs** (coder).
   - README:
     - a header with the logo (`docs/brand/mark.svg`), the name **Siphon**
       and the tagline;
     - the one-line "Formerly agentgw" note (`<!-- legacy-name -->`);
     - every command, path, option and unit renamed;
     - the flake URL `github:olafkfreund/siphon`.
   - `docs/adding-a-source.md` renamed the same way.
   - `.gitignore` entries renamed (`siphon.local.yaml`, `siphon.db`), and
     the old ones kept with `# legacy-name`.
4. **Ship** (Opus).
   - Move `~/.local/state/agentgw-dev` to `~/.local/state/siphon-dev` and
     rename its config to `siphon.yaml`. Restart the local instance from
     there.
   - Fresh review: a quick diff review on `model: opus` for anything the
     straggler test can't see, such as unit names split across strings.
   - Open the PR. Merge once CI is green and the owner approves.
   - **Ask the owner** for the go-ahead, then `gh repo rename siphon`.
     Then update the local `git remote`, and confirm the redirects and the
     flake URL.

## Tests

`go test -race ./...` (straggler, fallback, argv0 and schema tests),
`devenv test`, `nix build .#siphon .#agentgw`,
`nix build .#checks.x86_64-linux.legacy-option`,
`nix build .#checks.x86_64-linux.vm` (all subtests plus migration), and
CI green.

## Handoff

- **Coder:** steps 1 and 3 (a fresh `coder` agent, then `SendMessage`).
- **Opus:** steps 2 and 4.
- Steps 1 and 2 touch disjoint files, so they run in parallel; the VM test
  runs once both are in.

## Rollback

Revert the merge. Old deployments keep `/var/lib/agentgw` (the migration
copies, never moves), so a revert loses no data. Undo the repo rename with
`gh repo rename MCP-AgentGateway` if needed.

## Deviations log

- **From the spec:** the spec's units row says `StopOrphans` also stops
  leftover `agentgw-action*@` instances. The new polkit rule wouldn't allow
  that, and `nixos-rebuild switch` already stops units whose templates were
  removed. So no legacy orphan stop is kept (the spec's polkit row
  already says the old rule isn't kept).
