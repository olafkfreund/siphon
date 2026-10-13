---
status: approved
issue: 58
spec: spec/2026-10-13-58-cli-flake-install.md
---

# Plan: install the siphon CLI alone from the flake

## Approved decisions

- Documentation only. No flake, module or code changes.
  `packages.default` is `siphon`, and `meta.mainProgram` is `"siphon"`
  (checked: `nix eval .#packages.x86_64-linux.default.meta.mainProgram`
  gives `"siphon"`).
- Linux only (`x86_64-linux`, `aarch64-linux`), and the README says so.
  macOS is out of scope.
- No home-manager module and no `apps` output.
- `docs/cli.md` is generated, so don't edit it.
  `docs/getting-started.md` is part of `llms-full.txt`, so run
  `go generate ./docs` after editing it.
- Never show a token on a command line.

## Steps

1. **`README.md:181-182`: add `### Install the CLI` right after the
   `## CLI` heading, before the "`siphon` is both a client…" paragraph.**
   Content, in this order:
   - One sentence: for using the CLI on another machine than the daemon's.
     On the daemon's NixOS host, `cli.enable` already does it (see below).
   - An `sh` block:

     ```sh
     nix run github:olafkfreund/siphon -- help            # try it, nothing installed
     nix profile install github:olafkfreund/siphon        # install it for your user
     ```

   - A `nix` block with the flake input, and the package for NixOS
     (`environment.systemPackages`) or home-manager (`home.packages`):
     `inputs.siphon.packages.${pkgs.system}.default`.
   - An `sh` block: `siphon login https://siphon.example.com` with the
     comment `# the token is read from a no-echo prompt, or stdin`, then
     `siphon status`.
   - Three bullets:
     - `http://` only to loopback; a remote daemon needs https, see
       [reverse proxy](docs/tasks/reverse-proxy.md). The client refuses
       otherwise: `internal/client/client.go:205`.
     - In scripts: `SIPHON_URL` and `SIPHON_TOKEN_FILE`.
     - Linux only (`x86_64`, `aarch64`).

   → Verify: read the rendered section, then run `nix run .# -- help`
   and `nix run github:olafkfreund/siphon -- help`; both print the usage.

   Traps: no token in any example. Keep the wrapped lines in the README's
   existing style.

2. **`docs/getting-started.md:14`: add a row after the Docker/Podman row.**
   It reads: `| Siphon already running elsewhere, the CLI here |
   \`nix profile install github:olafkfreund/siphon\` ([Install the
   CLI](../README.md#install-the-cli)) |`. Then run `go generate ./docs`.

   → Verify: `go test ./docs/...` passes (it catches a stale
   `llms-full.txt`), and `git diff --stat` shows `llms-full.txt` changed.

   Traps: `docs_test.go` fails on stale generated files.

3. **Check the commands for real.**
   - `nix profile install .# --profile "$TMP/p"`, then `"$TMP/p/bin/siphon" help`.
     Afterwards, `rm -rf "$TMP/p"*`.
   - `siphon login http://10.0.0.1:8080 </dev/null` is refused with
     "refusing http:// …", and changes nothing.
   - `nix eval .#packages.x86_64-linux.default.meta.mainProgram` →
     `"siphon"`.

   → Verify: each one behaves as described. Fix the README if anything
   differs, and log the deviation here.

   Traps: use a throwaway profile, never the user's real one.

## Tests

- `go vet ./... && go test -race ./...`: green.
- The Step 3 commands behave as written.

## Rollback

Revert the commit: it touches `README.md`, `docs/getting-started.md` and
`llms-full.txt` only.
