---
status: draft
issue: 42
intent: intent/2026-10-09-42-remove-agentgw.md
---

# Spec: remove the legacy agentgw names

## Design

**Summary:** delete every shim marked `legacy-name`, replace the NixOS option
alias with a removal error, and make the straggler test strict. There is no
change in behaviour for anyone on `siphon` names, and no migration.

### Go

- **The binary:**
  - In `cmd/siphon/main.go:42-44`, drop the argv[0] notice.
  - `resolveConfig` (`main.go:149-163`) only exists for the `agentgw.yaml`
    fallback. Delete it, and pass the path straight through at its five call
    sites: `main.go:182`, `:274`, `:479`, `:564` and `backup.go:65`. The flag
    default stays `siphon.yaml`.
  - `cmd/siphon/legacy_test.go` is deleted. Both of its tests only cover the
    shims.
- **The database:**
  - In `internal/config/config.go`, `resolveDB` keeps the relative-path
    join. The `agentgw.db` fallback (`:513-520`), the `legacyDB` field
    (`:98`) and its warning (`:737-739`) go.
  - The comment at `:507` becomes "points a relative db at the config file's
    directory".
  - `TestLegacyDefaultDB` (`config_test.go:598`) is deleted.
- **The schema:**
  - `schema/agentgw.schema.json` is deleted.
  - `TestSchemaUpToDate` (`schema_test.go:18-19`) checks only
    `schema/siphon.schema.json`.
  - No document references the old file (checked with `grep`).
- **The straggler test:** in `internal/naming_test.go`:
  - drop the `legacy-name` line escape and the `schema/agentgw.schema.json`
    allowlist entry, so any "agentgw" in a tracked file outside `intent/`,
    `spec/`, `plan/` and `research/` fails;
  - the test's own string is written so that it doesn't match itself:
    `"agent" + "gw"`.

### Nix

- **`flake.nix`:**
  - drop `postInstall` (the symlink, `:38-39`);
  - drop `agentgw = siphon` (`:52`);
  - drop the stray `legacy-name` comment (`:171`);
  - replace the `legacy-option` check (`:192-213`) with `removed-option`.
    It evaluates a system that sets `services.agentgw.enable = true`, and
    asserts that `builtins.tryEval` on the system's top-level build fails.
    `tryEval` drops the error text, so the message itself is checked once
    by hand in the plan's tests. The option is set as
    `services.${"agent" + "gw"}.enable = true`, a dynamic attribute, so the
    strict straggler test doesn't match it.
- **`nix/module.nix`:**
  - the rename import (`:180-181`) becomes:

    ```nix
    imports = [
      (lib.mkRemovedOptionModule [ "services" ("agent" + "gw") ] "Siphon was renamed: use services.siphon.")
    ];
    ```

    The path is written as a split string so that the strict straggler test
    can't match it. The option name only exists on that line.
  - `migrateLegacyState` (`:161-177`) and its `"+${migrateLegacyState}"`
    entry in `ExecStartPre` (`:488`) are removed. Nothing reads or deletes
    `/var/lib/agentgw`. A `MIGRATED_FROM_AGENTGW` marker left on a host is
    harmless.
- **`nix/vm-test.nix`:** the "state from an old agentgw install is migrated"
  subtest (`:592-608`) is deleted.
- **`.github/workflows/ci.yml:25`:** builds
  `.#siphon .#checks.x86_64-linux.removed-option`.

### Other files

- **`README.md`:**
  - delete the "Formerly agentgw." line (`:10`) and the "Upgrading from
    agentgw" section (`:675-682`);
  - history stays in git and in the v0.5.0 release notes.
- **`.gitignore`:** drop the three `agent[g]w` entries and their comment.
  `*.db` already covers the database.
- **Docs and `llms-full.txt`:** regenerate only if a guide page changed.
  None is expected (checked with `grep`).

### v0.5.0 release notes (written at release, not in this PR)

> **Removed: the `agentgw` names** (promised for v0.2.0). If you still use
> them:
> - **NixOS:** rename `services.agentgw` to `services.siphon`. Evaluation
>   now fails with that message.
> - **Old state:** move the old state:
>   `systemctl stop siphon && mv /var/lib/agentgw/* /var/lib/siphon/ && chown -R siphon:siphon /var/lib/siphon`.
> - **Old file names:** rename `agentgw.yaml` to `siphon.yaml` and
>   `agentgw.db` to `siphon.db`.
> - **The binary:** the `agentgw` binary is gone. Call `siphon`.

### Before the merge: the owner's hosts (read-only)

- **The check:** search the owner's NixOS config repo for `services.agentgw`
  and `agentgw`. On the dev instance only, list `/var/lib/agentgw`, and only
  with permission. Nothing on any host is changed.
- **Reporting:** the results go into the PR description.

## Alternatives rejected

- **Keep the shims for one more release.** The promise was v0.2.0, and no
  release ever shipped under the old name. Waiting only adds another broken
  promise.
- **Drop `services.agentgw` silently.** Nix would report a generic unknown
  option. `mkRemovedOptionModule` costs one line and tells the user what to
  do (resolved question 1).
- **Keep the state migration.** No tagged release used `/var/lib/agentgw`
  (resolved question 2).
- **Keep `schema/agentgw.schema.json` as a redirect stub.** Nothing
  references it. A stub is a file to keep in sync for nobody.

## Risks

- **A pre-v0.1.0 install upgrades.** It would:
  - fail evaluation, if it sets `services.agentgw`, which is the intended
    loud failure;
  - start with an empty `/var/lib/siphon`, if it never migrated. The old
    directory is untouched, and the release notes give the `mv`.

  Mitigation: the host check before the merge, and the release notes.
- **The `removed-option` check only proves failure.** `tryEval` drops the
  message. That is still enough to prove the alias is gone, and the
  message is checked once by hand with `nix eval`.
- **A missed reference.** The strict straggler test catches any leftover
  "agentgw" in a tracked file.

## Verification

- **`go vet ./... && go test -race ./...`:** passes, including the strict
  `TestNoStragglers` and `TestSchemaUpToDate` with one file.
- **`grep -rni agentgw`** over tracked files outside `intent/`, `spec/`,
  `plan/` and `research/` finds nothing.
- **`nix flake check`:** passes, including `removed-option` and the VM test
  without the migration subtest.
- **`nix build .#agentgw`:** fails (no such attribute).
- **`nix build .#siphon`:** `result/bin` contains only `siphon`.
- **`devenv test`:** passes.
- **CI on the PR:** green.
