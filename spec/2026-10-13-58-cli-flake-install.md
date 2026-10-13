---
status: draft
issue: 58
intent: intent/2026-10-13-58-cli-flake-install.md
---

# Spec: install the siphon CLI alone from the flake

## Design

Documentation only. No flake or code changes: `packages.default` is
`siphon` with `meta.mainProgram = "siphon"` (`flake.nix:25-40`), so
`nix run` and `nix profile install` already work. Linux only, as decided
at approval.

**1. `README.md`, a new `### Install the CLI` at the top of `## CLI`
(`README.md:181`).** It is for using the CLI on a machine other than the
daemon's host:

```sh
nix run github:olafkfreund/siphon -- help            # try it, nothing installed
nix profile install github:olafkfreund/siphon        # install for your user
```

A flake input, for NixOS or home-manager:

```nix
inputs.siphon.url = "github:olafkfreund/siphon";
# NixOS:        environment.systemPackages = [ inputs.siphon.packages.${pkgs.system}.default ];
# home-manager: home.packages              = [ inputs.siphon.packages.${pkgs.system}.default ];
```

Then connect to the daemon:

```sh
siphon login https://siphon.example.com     # the token is read from a no-echo prompt, or stdin
siphon status
```

Three facts go in the section, each taken from the code:

- Plain `http://` is accepted only for loopback, so a remote daemon needs
  https. This links to `docs/tasks/reverse-proxy.md`.
- In scripts, set `SIPHON_URL` and `SIPHON_TOKEN_FILE` instead of saving a
  login.
- It is Linux only (`x86_64`, `aarch64`). The flake doesn't build for
  macOS.

The existing NixOS paragraph (`cli.enable`) stays where it is, after the
new subsection.

**2. `docs/getting-started.md:8-14`:** one row is added to the "You have"
table: "Siphon running elsewhere, the CLI here" →
`nix profile install github:olafkfreund/siphon`, linking to the README
section. Because `getting-started.md` is part of `llms-full.txt`, that
file is regenerated with `go generate ./docs`.

`docs/cli.md` is generated from `siphon help --json` and is not edited.

## Alternatives rejected

- **A home-manager module (`programs.siphon`):** it would only set
  `SIPHON_URL`, and `siphon login` already saves that. Ruled out at intent
  approval.
- **An `apps.siphon` flake output:** `nix run` already uses
  `packages.default` with `mainProgram`.
- **Adding darwin to `systems`:** out of scope by decision. It would need a
  build check on macOS that we can't run.

## Risks

- **A doc command that doesn't work:** each command is run before commit
  (see Verification). The flake-input snippet can't be run as written, so
  it is checked with an eval instead.
- **A stale `llms-full.txt`:** `docs_test.go` fails if it isn't
  regenerated.

## Verification

- `nix run .# -- help` and `nix run github:olafkfreund/siphon -- help`
  print the usage.
- `nix profile install .#` into a throwaway profile
  (`--profile $TMP/p`) puts `$TMP/p/bin/siphon` there.
- The snippet's attribute path evaluates:
  `nix eval .#packages.x86_64-linux.default.meta.mainProgram` gives
  `"siphon"`.
- `siphon login https://…`, given the token on stdin, against a local
  daemon. To test the https rule, `siphon login http://10.0.0.1:8080`
  must be refused (not loopback).
- `go generate ./docs && go test ./docs/...` pass.
