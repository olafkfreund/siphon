---
status: draft
issue: 58
author: olafkfreund
---

# Intent: install the siphon CLI alone from the flake

## Problem

The flake already builds the CLI (`packages.default = siphon`, one binary
for the daemon and the client). The NixOS module puts it on PATH with
`SIPHON_URL` set (`cli.enable`). But the README only documents ways to
*run* Siphon: the module, the microVM and the container image. A user who
wants the CLI on a laptop, to drive a daemon running elsewhere, has to
work out the install and the login on their own.

## Proposed outcome

The README has a short "Install the CLI" section that a reader can copy:

- try it without installing: `nix run github:olafkfreund/siphon -- …`;
- install it: `nix profile install github:olafkfreund/siphon`;
- declare it: the flake as an input, then the package in
  `environment.systemPackages` (NixOS) or `home.packages` (home-manager);
- connect to a daemon: `siphon login <url>`, with the token taken from a
  no-echo prompt or stdin, never a flag (or `SIPHON_URL` and
  `SIPHON_TOKEN_FILE`).

Each command is checked against the real flake before it goes in.

## Affected users and systems

- `README.md`, maybe a line in `docs/getting-started.md` and `docs/cli.md`.
- No code and no flake changes unless a check shows one is needed.

## Constraints

- Never show a token on a command line.
- Don't add a home-manager module or new flake outputs: the existing
  package covers it.

## Open questions

- Is the CLI-only path also meant for macOS (`aarch64-darwin`)? `forAll`
  decides which systems the package builds on. If darwin isn't in it, that
  is a flake change and grows this task.
