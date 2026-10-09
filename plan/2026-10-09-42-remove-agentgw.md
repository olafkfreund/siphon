---
status: draft
issue: 42
spec: spec/2026-10-09-42-remove-agentgw.md
---

# Plan: remove the legacy agentgw names

## Approved decisions (from the intent and the spec)

- **Remove every `legacy-name` shim:**
  - the `agentgw` binary symlink and the `.#agentgw` package;
  - the argv[0] notice;
  - the `agentgw.yaml` and `agentgw.db` fallbacks;
  - the `/var/lib/agentgw` state migration and its VM subtest;
  - `schema/agentgw.schema.json`;
  - the README's "Formerly" line and upgrade section;
  - the `.gitignore` entries;
  - the CI builds of `.#agentgw` and `legacy-option`.
- **The old option fails loudly.** `services.agentgw` becomes
  `lib.mkRemovedOptionModule [ "services" ("agent" + "gw") ] "Siphon was renamed: use services.siphon."`.
  A flake check, `removed-option`, asserts that evaluating a system that
  sets it fails. That check sets the option as
  `services.${"agent" + "gw"}.enable = true`.
- **Never touch state.** Nothing reads or deletes `/var/lib/agentgw`.
- **The straggler test becomes strict:**
  - no `legacy-name` escape;
  - no schema allowlist entry;
  - only `intent/`, `spec/`, `plan/` and `research/` may mention the old
    name;
  - the test spells it `"agent" + "gw"`.
- **Before the merge:** a read-only search of the owner's NixOS config for
  the old name. Hosts are listed only with permission. Nothing is changed.
- **Release notes:** written at the v0.5.0 release, not in this PR (the
  text is in the spec).

## Steps

1. **Go removals.** Done by the coder.
   - **`cmd/siphon/main.go`:**
     - delete the argv[0] notice (lines 42-44);
     - delete `resolveConfig` (lines 149-163, with its doc comment);
     - at `main.go:182`, `:274`, `:479`, `:564` and `cmd/siphon/backup.go:65`,
       use the path directly (`*path`, `cfgPath`);
     - drop the `flag` and `filepath` imports only if they're now unused.
   - **Tests:** delete `cmd/siphon/legacy_test.go`.
   - **`internal/config/config.go`:**
     - delete the `legacyDB` field (line 98);
     - in `resolveDB` (lines 506-521), delete the `agentgw.db` fallback,
       keep the relative-path join, and make the doc comment "resolveDB
       points a relative db at the config file's directory.";
     - in `Warnings` (lines 737-739), delete the `legacyDB` warning;
     - drop `errors` and `os` only if they're now unused.
   - **`internal/config/config_test.go`:** delete `TestLegacyDefaultDB`
     (from the comment at line 598).
   - **Schema:**
     - in `internal/config/schema_test.go` (lines 18-19), loop over only
       `../../schema/siphon.schema.json`, or unroll to a single path, and
       drop the legacy comment;
     - delete `schema/agentgw.schema.json` with `git rm`.
   - **Verify:** `go vet ./... && go test -race ./...`.
   - **Traps:**
     - leave `internal/naming_test.go` alone; it is step 4;
     - don't commit; the coder can't.
2. **Nix and CI.** Done by Opus.
   - **`flake.nix`:**
     - delete `postInstall` and its comment (lines 38-39);
     - delete `agentgw = siphon;` (line 52);
     - delete the stray comment at line 171;
     - replace `legacy-option` (lines 192-213) with `removed-option`:

       ```nix
       removed-option =
         let
           sys = nixpkgs.lib.nixosSystem { …same modules…, services.${"agent" + "gw"}.enable = true; };
         in
         assert !(builtins.tryEval sys.config.system.build.toplevel.drvPath).success;
         pkgs.runCommand "removed-option-ok" { } "touch $out";
       ```
   - **`nix/module.nix`:**
     - delete `migrateLegacyState` (lines 161-177, with its comment);
     - delete its `"+${migrateLegacyState}"` entry in `ExecStartPre`
       (around line 488);
     - replace the import at lines 180-181 with the `mkRemovedOptionModule`
       line above.
   - **`nix/vm-test.nix`:** delete the subtest "state from an old agentgw
     install is migrated" (lines 592-608).
   - **`.github/workflows/ci.yml:25`:**
     `nix build -L .#siphon .#checks.x86_64-linux.removed-option # also proves vendorHash is current`.
   - **Verify:**
     - `nix build .#checks.x86_64-linux.removed-option .#siphon`;
     - `ls result*/bin`;
     - `nix eval` of a system with the old option prints the "Siphon was
       renamed" message (checked by hand once);
     - `nix build .#agentgw` fails.
   - **Traps:**
     - the VM script is type-checked;
     - don't put `systemctl restart` in Bash text;
     - `tryEval` only catches `throw` and `assert`, and
       `mkRemovedOptionModule` uses an assertion, so evaluate
       `toplevel.drvPath`.
3. **Docs.** Done by Opus.
   - **`README.md`:** delete line 10 ("Formerly agentgw.") and the
     "Upgrading from agentgw" section (lines 675-682, with the blank line
     after it).
   - **`.gitignore`:** delete the legacy comment and the three
     `agent[g]w` lines.
   - **Verify:** `go test ./docs/ ./internal/web/`. Regenerate
     `llms-full.txt` only if a docs page changed.
   - **Traps:** none.
4. **Strict straggler test.** Done by Opus.
   - **`internal/naming_test.go`:**
     - match on `old := "agent" + "gw"`, case-insensitively;
     - drop the `legacy-name` escape;
     - drop the schema entry from `allowedFile`.
   - **Verify:**
     - `go test ./internal/`;
     - `git grep -il agentgw -- ':!intent' ':!spec' ':!plan' ':!research'`
       is empty.
   - **Traps:** the test file must not contain the literal name.
5. **Host check and PR.** Done by Opus.
   - **The host check:** search the owner's NixOS config repo for
     `agentgw`, read-only. Put the result in the PR description.
   - **Then:**
     - run `nix flake check`;
     - push;
     - open a draft PR linking intent, spec and plan, saying step 1 was the
       coder's.
6. **Security review.** A fresh Opus agent, given only this plan and the
   diff. Fix each finding or record it here.

## Tests

- **`go vet ./... && go test -race ./...`:** passes.
- **`nix flake check`:** passes, including `removed-option` and the VM test.
- **`git grep -il agentgw`** outside `intent/`, `spec/`, `plan/` and
  `research/`: empty.
- **`nix build .#agentgw`:** fails with a missing attribute.
- **CI:** green.

## Rollback

- **Revert the merge.** No state is touched. A host on the old names gets
  the shims back.

## Deviations

None yet.
