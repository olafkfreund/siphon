---
status: approved
issue: 14
author: olafkfreund
---

# Intent: Run agentgw as a microVM and as a Docker/OCI image

## Problem

agentgw can be run in only two ways today:

- **The NixOS module.** This gives the full security model: each run in its
  own `agentgw-action@` systemd unit, a DynamicUser, a private network
  namespace and the egress allowlist. But it means changing the host's
  system configuration and rebuilding it. That is a lot to ask just to try
  agentgw, and impossible on a host that isn't NixOS.
- **The bare binary with `sandbox: none`.** This is easy, but agents run
  unsandboxed with the operator's logins and nothing enforces egress. It's
  good for a look, not for real use.

Nothing sits in between. An operator can't get the real sandbox without
touching the host OS, and anyone not on NixOS (or using podman/docker, k8s,
or a home-lab container host) has no way to run it at all.

## Proposed outcome

1. **A microVM.** One command (for example `nix run .#microvm`) boots a small
   NixOS VM running agentgw through the NixOS module, so it has the full
   sandbox and egress restriction.
   - The portal and webhooks are reachable from the host.
   - The config, state DB and imported logins live on a host directory
     that survives reboots and rebuilds.
   - The host OS is not changed; stopping the VM stops everything.
   - It works on p620 under the user's own account.
2. **A Docker/OCI image.**
   - Built reproducibly from the flake, small, and with no shell needed to
     run.
   - Runs under rootless podman and under docker: config mounted in, state
     on a volume, portal on a published port.
   - The image is honest about its limits. Inside a plain container there
     is no systemd sandbox, so it runs `sandbox: none`.
   - Startup and `validate` say clearly that runs are not isolated and
     egress is not enforced, and the README states which deployment gives
     which guarantees.
3. **One config file works in all three modes** (module, microVM, image),
   apart from the documented differences such as `sandbox` and the
   listen address.
4. **Tested.** CI builds both artifacts. A check boots the microVM and runs
   a rule end to end, and a check starts the image and serves `/healthz`
   and a webhook-triggered `cmd` rule.
5. **Documented.** The README has a short "Ways to run agentgw" table
   (module / microVM / image), with the isolation each one gives and the
   commands to start each.

## Affected users and systems

- **Operators** who want to try agentgw without changing their host, or who
  run podman/docker rather than NixOS.
- **The repo:**
  - `flake.nix`: new outputs (a microVM runner, an image), possibly a new
    flake input;
  - `nix/` (the microVM's NixOS configuration);
  - the CI workflow;
  - the README.
- **p620 (local testing):** only user-level VM and podman processes. No
  change to its NixOS configuration.

## Constraints

- **Must not weaken the sandbox story.** The image must never claim
  isolation it doesn't have. `sandbox: systemd` inside a container must fail
  with a clear error rather than half-work.
- **The microVM must use the same NixOS module and the same tests** as a real
  host. No second, divergent sandbox implementation.
- **Secrets are never baked into an image or a VM image.** The token, webhook
  secrets and logins come in at runtime: mounted files, env, or
  `agentgw credentials import` against the persistent state.
- **The image must not redistribute unfree software.** claude-code is
  unfree, so the agent CLIs probably can't go in a published image
  (open question 2).
- **Apache-2.0 compatible**, no AGPL code. A new flake input (for example
  microvm.nix, MIT) is acceptable only if it earns its weight (open question 1).
- **No changes to production hosts.** p620 is used only through user-level
  processes.

## Open questions

1. **microVM tooling.** Options:
   - `microvm.nix`: a new input, fast boot, virtiofs shares, made for this.
   - The stock NixOS `virtualisation.vmVariant` / `qemu-vm` (what the VM
     test uses): no new input, slower to boot, but already proven here.
2. **Agent CLIs in the image.** Options:
   - (a) Ship none, and the operator layers their own on top (a documented
     `FROM`);
   - (b) ship only the freely redistributable ones (codex);
   - (c) publish two variants.

   claude-code being unfree rules out bundling it in a public image.
3. **Publishing.** Should CI push the image to `ghcr.io/olafkfreund/agentgw` on
   tags (v0.1.0 onwards), or only build it locally and in CI for now?
4. **Some isolation inside the image?** Running runs under bubblewrap inside
   the container (needs user namespaces, which rootless podman may block) is
   a possible later step. Proposal: out of scope here, and the image stays
   `sandbox: none` and says so.
5. **Architectures.** x86_64-linux only, or aarch64-linux as well (for
   Raspberry Pi and ARM home labs)?

## Decisions at approval (2026-10-06)

The owner approved the proposals:
1. microvm.nix.
2. No agent CLIs in the published image; a documented `FROM` adds your
   own.
3. Push to `ghcr.io` on version tags.
4. No in-container isolation for now: the image runs `sandbox: none` and
   says so.
5. Architectures: x86_64-linux and aarch64-linux.

The project is being renamed to Siphon (#16), so the outputs use the new
name (`siphon`).
