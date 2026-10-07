---
status: approved
issue: 14
intent: intent/2026-10-06-14-microvm-oci-image.md
---

# Spec: Run Siphon as a microVM and as a Docker/OCI image

**Precondition:** this lands after #16 (rename to Siphon). The names here
are post-rename.

## Design

### Three ways to run, one config

| Mode | Command | Isolation | For |
|---|---|---|---|
| **NixOS module** | `services.siphon.enable = true` | Full: template units, DynamicUser, private netns, egress allowlist | Real deployments on NixOS |
| **microVM** | `nix run github:olafkfreund/siphon#microvm` | Full (the same module inside a small NixOS VM); host untouched | Trying the real thing, or any Linux host with KVM |
| **OCI image** | `podman run … ghcr.io/olafkfreund/siphon` | **None**: `sandbox: none`, egress not enforced | Portal, rules, sources and commands on any container host |

The README gets a "Ways to run Siphon" section with this table, the
commands, and a clear statement of what the image does not isolate.

### microVM (microvm.nix, MIT; new flake input)

- **Definition.** `nixosConfigurations.siphon-microvm`, defined in
  `nix/microvm.nix`:
  - imports `microvm.nixosModules.microvm` and our `nixosModules.default`;
  - hypervisor qemu, 2 vCPUs, 2 GiB of RAM, user (SLiRP) networking, so it
    needs no root, no bridge and no host network changes.
- **The portal on the host.** `microvm.forwardPorts` maps host
  `127.0.0.1:8090` to guest `8080`. Override it with `SIPHON_VM_PORT`; the
  runner script reads it at start.
- **Config from the host.**
  - A `9p` share (works unprivileged) of `$SIPHON_VM_DIR` (default
    `~/.config/siphon-vm`) is mounted read-only at `/etc/siphon` in the
    guest.
  - It holds `siphon.yaml` and any secret files the config references as
    `file:/etc/siphon/...`. The runner creates the directory with a
    starter `siphon.yaml` and a random token on first run, and prints
    where they are.
- **A new module option,** `services.siphon.configFile`
  (`nullOr str`, default null). When set, it replaces the generated file,
  and `validate` runs against it at start. It is useful outside the
  microVM too, for a config managed outside Nix.
  - The microVM sets `configFile = "/etc/siphon/siphon.yaml"` and forces
    `server.listen = "0.0.0.0:8080"` inside the guest through an env
    override.
  - Precedence: the env var `SIPHON_LISTEN`, if set, overrides
    `server.listen`. It is documented, and applies to the image too.
- **State that persists.** `microvm.volumes` uses a 2 GiB ext4 image at
  `$SIPHON_VM_DIR/state.img`, mounted at `/var/lib`. The DB, imported logins
  and the portal overlay survive restarts and rebuilds.
- **Logins.**
  - Through the portal's "Add login" (#15).
  - Until #15 lands, through the VM's serial console:
    `siphon credentials import …`. The runner's banner prints the exact
    command.
- **Agent CLIs.**
  - `services.siphon.agentPackages` in the microVM defaults to
    `[ pkgs.codex ]`.
  - `claude-code` is unfree, so it is added only when the user's local
    build allows it. `nix/microvm.nix` takes `allowUnfreeAgents ? false`,
    and a second output `#microvm-unfree` passes `true`. These are local
    builds, so nothing unfree is published.
- **Egress, sandbox and polkit** are exactly the module's: the guest is a
  normal NixOS host.
- **The runner:** `packages.<sys>.microvm` = a small wrapper around
  `config.microvm.declaredRunner` that:
  - prepares `$SIPHON_VM_DIR`;
  - creates `state.img` on first run;
  - prints the URL, token path and console hint;
  - starts the VM.

  Ctrl-C (or `shutdown` in the console) stops it.
- **Requirements:** Linux with KVM (`/dev/kvm` readable by the user).
  Without KVM, the runner says so and exits (no slow TCG fallback by
  default; `SIPHON_VM_NOKVM=1` forces TCG).

### OCI image (`dockerTools`)

- **Build.**
  - `packages.<sys>.image` = `dockerTools.streamLayeredImage`, named
    `ghcr.io/olafkfreund/siphon`, tagged with the version.
  - Contents: `siphon`, `cacert`, `tzdata`, and nothing else (no shell).
  - It runs as UID/GID 65532 (nonroot), with
    `Entrypoint ["siphon"]`, `Cmd ["serve","-config","/etc/siphon/siphon.yaml"]`,
    `WorkingDir /var/lib/siphon`, volume `/var/lib/siphon`, `EXPOSE 8080`.
  - Env: `SIPHON_LISTEN=0.0.0.0:8080` and
    `SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt`.
  - OCI labels: source, licence and version.
- **Honest isolation.**
  - Siphon detects a container (`/run/.containerenv`, `/.dockerenv`, or
    `container=` in the environment).
  - In a container, `server.sandbox` **defaults to `none`**. If it is
    explicitly `systemd`, `validate` **errors**: "systemd sandbox is not
    available inside a container; use the NixOS module or the microVM for
    isolation".
  - Startup logs a prominent warning, and the portal shows a permanent
    "Unsandboxed: runs are not isolated and egress is not enforced"
    banner.
- **Agent CLIs.** None in the published image.
  - `lib.<sys>.mkImage { agentPackages = [ pkgs.claude-code pkgs.codex ]; }`
    (flake output) builds a custom image locally with them added. It is
    documented in the README.
  - A plain `Containerfile` `FROM` example is documented too, for people
    without Nix (installing the CLIs with their own vendor instructions).
- **Architectures.** x86_64-linux and aarch64-linux, each built natively in
  CI (`ubuntu-24.04` and `ubuntu-24.04-arm` runners).
- **Publishing.** On `v*` tags, CI pushes both images to ghcr with
  `skopeo` and then a multi-arch manifest (`:vX.Y.Z` and `:latest`). It
  uses `GITHUB_TOKEN` with `packages: write`. Pull requests build both
  images but don't push.
- **Run (documented):**
  ```sh
  podman run -d --name siphon -p 127.0.0.1:8090:8080 \
    -v ~/.config/siphon:/etc/siphon:ro,Z -v siphon-state:/var/lib/siphon \
    ghcr.io/olafkfreund/siphon:latest
  ```
  Rootless podman and docker both work. The image needs no capabilities
  (`--cap-drop=all --read-only` is documented as recommended; the state
  volume and `/tmp` tmpfs are the only writable paths).

## Alternatives rejected

- **The stock `virtualisation.vmVariant` / qemu-vm** instead of
  microvm.nix: no new input, but slower boot, awkward persistent volumes,
  and port forwarding through `QEMU_NET_OPTS`. The owner chose microvm.nix.
- **systemd inside the container** (podman `--systemd=always` with the
  module): fragile under rootless (cgroup delegation, BPF IP filters,
  `PrivateNetwork`, polkit), and it would claim isolation it can't
  guarantee.
- **bubblewrap isolation inside the container:** rootless podman often
  blocks the user namespaces it needs. It is out of scope here (intent
  Q4) and can be revisited.
- **A Dockerfile build** (`FROM golang` + `FROM distroless`): a second build
  path to keep in sync. `dockerTools` reuses the flake's exact binary and
  needs no Docker daemon.
- **Publishing on every main commit:** noisy. Tags only (intent Q3).

## Risks

| Risk | Mitigation |
|---|---|
| microvm.nix input churn breaks the build | Pinned in `flake.lock`; CI builds the runner; the module itself does not depend on it |
| 9p share performance or permissions (UID mapping) | Read-only config and small secret files only; state lives on the volume, not the share |
| KVM missing on CI runners | The existing `vm` job enables KVM (udev rule); the microVM boot check runs there |
| Image users assume isolation | Validate error for `sandbox: systemd`, startup warning, permanent portal banner, README table |
| `SIPHON_LISTEN` overriding config surprises someone | Documented, logged at start ("listen overridden by SIPHON_LISTEN") |
| aarch64 runner availability or cost | Native ARM runners are free for public repos; fall back to building aarch64 through the binfmt/QEMU builder if needed |

## Verification

- `nix flake check`: the `image` and `microvm` outputs build on x86_64 (and
  aarch64 in CI).
- **Container check (CI, podman):**
  - load the image and run it with a test config (a webhook source plus a
    rule whose cmd is `["siphon","version"]`, since the image has no
    shell);
  - `GET /healthz` returns 200;
  - fire a signed webhook, and the job ends `done` with the version in
    its output;
  - `validate` with `sandbox: systemd` inside the container fails with
    the message;
  - the container runs as UID 65532 with `--cap-drop=all --read-only`.
- **microVM check (CI `vm` job, KVM):**
  - start the runner with a temp `SIPHON_VM_DIR` holding a test config;
  - wait for `127.0.0.1:8090/healthz`;
  - fire a webhook, and the cmd job is `done`;
  - stop the VM and start it again: the job is still listed (the volume
    persisted).
- **Unit tests:** container detection and the sandbox default/error;
  the `SIPHON_LISTEN` override; the `configFile` module option
  (an eval check).
- **Owner:** on p620, `nix run .#microvm` (or `#microvm-unfree`) and the
  portal at http://127.0.0.1:8090; `podman run` per the README.
