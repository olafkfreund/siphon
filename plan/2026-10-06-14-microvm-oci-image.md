---
status: approved
issue: 14
spec: spec/2026-10-06-14-microvm-oci-image.md
---

# Plan: Run Siphon as a microVM and as a Docker/OCI image

## Approved decisions (self-contained)

- **Three ways to run, one config:**
  - the **NixOS module** (full isolation);
  - the **microVM**, `nix run .#microvm` (full isolation, host untouched);
  - the **OCI image**, `ghcr.io/olafkfreund/siphon` (**no isolation**:
    `sandbox: none`, egress not enforced).

  The README gets a "Ways to run Siphon" table.
- **Container honesty.**
  - Siphon detects a container (`/run/.containerenv`, `/.dockerenv`, or
    `container=` in the env).
  - In a container `server.sandbox` defaults to `none`, and an explicit
    `systemd` is a `validate` **error**: "systemd sandbox is not available
    inside a container; use the NixOS module or the microVM for isolation".
  - Startup logs a warning, and the portal shows a permanent "Unsandboxed:
    runs are not isolated and egress is not enforced" banner.
- **`SIPHON_LISTEN`** (env) overrides `server.listen`, and this is logged
  at start. It's used by the image and the microVM.
- **`services.siphon.configFile`** (`nullOr str`): when set, it replaces
  the generated config file (validated with `-file-only` at start). The
  microVM uses `/etc/siphon/siphon.yaml`.
- **The image:**
  - `dockerTools.streamLayeredImage` containing `siphon`, `cacert`, `tzdata`
    and nothing else, with no shell;
  - user 65532:65532, `Entrypoint ["siphon"]`,
    `Cmd ["serve","-config","/etc/siphon/siphon.yaml"]`,
    `WorkingDir /var/lib/siphon`, volume `/var/lib/siphon`, `EXPOSE 8080`;
  - env `SIPHON_LISTEN=0.0.0.0:8080` and `SSL_CERT_FILE`;
  - OCI labels.
  - `lib.<sys>.mkImage { agentPackages = [ … ]; }` builds a local variant
    with agent CLIs. The published image has none.
  - Recommended run flags: `--cap-drop=all --read-only` with a tmpfs
    `/tmp`.
- **The microVM** (microvm.nix, MIT, a new flake input):
  - `nixosConfigurations.siphon-microvm` with qemu, 2 vCPUs, 2 GiB and SLiRP
    networking, with `forwardPorts` mapping host `127.0.0.1:${SIPHON_VM_PORT:-8090}`
    to guest 8080;
  - a read-only 9p share of `$SIPHON_VM_DIR` (default `~/.config/siphon-vm`)
    at `/etc/siphon`, plus a 2 GiB `state.img` volume at `/var/lib`;
  - `configFile = "/etc/siphon/siphon.yaml"`;
  - agentPackages `[ codex ]`, with `#microvm-unfree` adding claude-code
    for local builds only;
  - a runner wrapper `packages.microvm` that prepares the dir (a starter
    `siphon.yaml` plus a random token), creates `state.img` on first run,
    prints the URL, token path and console hint, checks `/dev/kvm` (with
    `SIPHON_VM_NOKVM=1` forcing TCG), then runs the declared runner.
- **Architectures and publishing:** x86_64 and aarch64, built natively in CI
  (`ubuntu-24.04` and `ubuntu-24.04-arm`). On `v*` tags, CI pushes to ghcr
  with `skopeo` and a multi-arch manifest (`:vX.Y.Z`, `:latest`). Pull
  requests build without pushing.

## Steps

Each step is one commit; cite "Plan step N". Run `go test -race ./...` and
`devenv test` after Go steps, and `nix flake check` (or the relevant
`nix build`) after Nix steps.

| # | Step | Who | Lane |
|---|---|---|---|
| 1 | Container detection, the sandbox default and its error, `SIPHON_LISTEN`, the startup warning, the portal banner | coder | `internal/config`, `cmd/siphon`, `internal/web` (the banner only) |
| 2 | Module `configFile` option, plus an eval check | Opus | `nix/module.nix`, `flake.nix` |
| 3 | OCI image + `lib.mkImage` + a CI container check (podman) | Opus | `flake.nix`, `nix/image.nix`, `.github/workflows/ci.yml` |
| 4 | microVM: flake input, nixosConfigurations (+ unfree variant), runner wrapper, CI boot check | Opus | `flake.nix`, `flake.lock`, `nix/microvm.nix`, CI |
| 5 | Tag publishing workflow (multi-arch ghcr) + README "Ways to run Siphon" | Opus | `.github/workflows/release.yml`, README |
| 6 | Review, owner live checks (`nix run .#microvm` and `podman run` on p620), PR | Opus | none |

1. **Container honesty** (coder).
   - `config.InContainer()`: checks for `/run/.containerenv` or
     `/.dockerenv`, or a `container` env var. It's injectable for tests.
   - In `Parse` defaults: if in a container and `sandbox` is unset, use
     `none`. `Validate`: in a container with `sandbox: systemd`, error with
     the exact message.
   - `cmd/siphon serve`: if `SIPHON_LISTEN` is set, override
     `server.listen` and log `listen overridden by SIPHON_LISTEN`. Log a
     warning when `sandbox: none` in a container.
   - `web.Options.Unsandboxed bool` renders a permanent banner in the
     layout. It shows whenever `sandbox: none` (container or not), since
     the local dev instance deserves it too.
   - **Tests:** the detection seam; the default; the error; the env
     override; the banner rendered and CSP-clean.
2. **`configFile`** (Opus).
   - `services.siphon.configFile = mkOption { type = nullOr str; default = null; }`.
   - When it's set, ExecStart and ExecStartPre use it instead of the
     generated file, and `settings` is ignored (an assertion warns if both
     are set).
   - `checks.<sys>.config-file`: an eval of a system with `configFile` set
     shows the path in the unit.
3. **Image** (Opus).
   - `nix/image.nix` provides `{ pkgs, siphon, agentPackages ? [] }:` and
     `dockerTools.streamLayeredImage` as decided.
   - `packages.image` and `lib.mkImage`.
   - **CI job `container`:**
     1. `nix build .#image && ./result | podman load`;
     2. run it with a test config (a webhook source plus a rule with
        `cmd: ["siphon","version"]`) using `--cap-drop=all --read-only`
        and a tmpfs;
     3. `curl /healthz` returns 200;
     4. a signed webhook leads to a job that is `done` and whose output has
        the version;
     5. `podman run … siphon validate` with `sandbox: systemd` exits non-zero
        with the message;
     6. `id` (UID 65532) is checked through `podman inspect`.
4. **microVM** (Opus).
   - The flake input `microvm.url = "github:microvm-nix/microvm.nix"`, with
     `inputs.nixpkgs.follows = "nixpkgs"`.
   - `nix/microvm.nix` is a function of `{ allowUnfreeAgents ? false }`.
   - The `nixosConfigurations.siphon-microvm` and `-unfree` outputs, and
     `packages.microvm` and `packages.microvm-unfree` (the wrappers).
   - **CI** (the `vm` job, KVM): start the wrapper with a temp
     `SIPHON_VM_DIR` holding a test config; wait for `127.0.0.1:8090/healthz`;
     fire a webhook so a `cmd` job is `done`; power off and start again; the
     job is still listed. Timeout 10 min.
   - **Trap:** the 9p share is read-only, so `siphon` must not write next
     to the config. The DB lives on the `/var/lib` volume, and the module
     already sets `server.db` under `/var/lib/siphon`.
5. **Publishing and docs** (Opus).
   - `.github/workflows/release.yml` on `v*` tags: a build matrix for
     x86_64 and aarch64, `./result | skopeo copy docker-archive:/dev/stdin docker://ghcr.io/…:<tag>-<arch>`,
     then `skopeo` or `docker manifest` to create `:<tag>` and `:latest`.
     Permissions `packages: write`. Pull requests: build only, in `ci.yml`.
   - README "Ways to run Siphon": the table, the commands, and the
     isolation notes.
6. **Review, live and PR** (Opus).
   - A fresh review focused on the image (no shell, user, labels, no
     secrets baked in), the honesty of the container mode, and the
     microVM's shares and volume permissions.
   - Owner live on p620:
     - `nix run .#microvm`: the portal opens at
       http://127.0.0.1:8090, and a model agent on the host's Ollama works
       from inside the VM. That needs `private_endpoints` for the host IP as
       seen from the VM, `10.0.2.2:11434` (SLiRP gateway), documented.
     - `podman run …`: the portal opens and shows the Unsandboxed banner.
   - Then the PR.

## Tests

`go test -race ./...`, `devenv test`, `nix flake check`, the CI
`container` and microVM jobs, the NixOS VM test (unchanged, still green),
and the owner's live checks.

## Handoff

- **Coder:** step 1 (Go).
- **Opus:** steps 2–6 (Nix, CI and docs).
- Steps 2–4 can start in parallel with step 1. Step 3's CI check needs step
  1 for the container error.

## Rollback

Revert the merge. The NixOS module behaviour without `configFile` is
unchanged. Remove the microvm flake input with the revert.

## Deviations log
- **Step 2 (Opus):** with `configFile` set, `settings` is ignored (with a warning), and the file must set `server.db`, `actions_dir` and `egress.socket` itself (documented on the option). Eval check `checks.<sys>.config-file`.
- **Step 3 (Opus):**
  - The image is ~23 MB.
  - Env includes `container=oci` and a `PATH` for any added agent packages.
  - **Mounted config files must be world-readable** (uid 65532 inside; with rootless podman the host owner maps to the container's root). The README says so.
  - The CI check is `ci/container-test.sh`, which passed locally and also exercises step 1's validate error.
- **Step 4 (Opus):**
  - **`qemu.machine = "q35"`,** not microvm.nix's default `microvm` machine: that hung in early kernel boot (decompression, 100% CPU) on the owner's AMD Threadripper 3995WX. q35 boots in about 8 s.
  - **The port is fixed at 127.0.0.1:8090**, since microvm.nix bakes `forwardPorts` at build time. `SIPHON_VM_PORT` was dropped, and so was `SIPHON_VM_NOKVM`: the runner always passes `-enable-kvm`, and the wrapper refuses to start without `/dev/kvm`.
  - The shared `config/` dir is 0755 inside a 0700 `$SIPHON_VM_DIR`, so the guest's siphon user can read it over 9p and other host users can't. An earlier 0700 made the config unreadable in the guest.
  - Verified locally: boot, portal 200, a webhook job is done, a restart keeps the job (the `state.img` volume). The CI check is `ci/microvm-test.sh` in the `vm` job.
- **Step 1 (coder):**
  - `config.InContainer` is a func var (a seam).
  - The container default and the `systemd` error are in parse and `Validate`.
  - `applyListenEnv` handles `SIPHON_LISTEN`.
  - The Unsandboxed banner is set at startup from `sandbox == none`. `server.*` is file-only, so it can only change with a restart.
- **Step 6 review fixes (Opus).** A fresh security review found one high, five medium and six low issues. Fixed:
  - **H1:** with `configFile`, the config's directory is added to the action units' `InaccessiblePaths`, so runs cannot read the portal token or webhook secrets. `ci/microvm-test.sh` now proves a run's `cat /etc/siphon/token` fails.
  - **M1:** the 9p share is `readOnly = true`, as the plan said. The wrapper uses `umask 077` and `noclobber`, refuses to start if the directory contains a symlink, and chmods only regular files.
  - **M2:** `siphon-action-open@` in the microVM denies `10.0.2.2` (the host's loopback through SLiRP). Restricted runs only reach the egress proxy, which refuses private addresses unless they are listed.
  - **M3:** `release.yml`:
    - tag, actor and owner reach the shell as env, and the tag is regex-checked;
    - `skopeo login --password-stdin` replaces the token in argv;
    - tools come from `nix shell --inputs-from .`;
    - actions are pinned by SHA (in `ci.yml` too), with `persist-credentials: false`;
    - `packages: write` is set per job.
  - **M4:** the README and `ci/container-test.sh` use `--userns=keep-id:uid=65532,gid=65532` with 0600 files, never world-readable ones. This supersedes step 3's "world-readable" note.
  - **M5:** `configFile` must be absolute, in its own directory, and outside the store and `/var/lib/siphon*` and `/run/siphon` (an assertion). The warning now says `settings.units` still sets the polkit allowlist. The `egress.socket`/`egress.enable` pairing is documented on the option; it can't be checked at eval time.
  - **L1:** `SIPHON_LISTEN` is applied in `loadCfg`, before the warnings.
  - **L2:** the generated config sets `server.sandbox: systemd`.
  - **L3:** the image has no `PATH` entry without agent packages.
  - **L5:** `release.yml` runs `ci.yml` (`workflow_call`) before publishing, and pre-release tags (`v1.0.0-rc1`) don't move `:latest`.
  - **L6:** documented in `nix/microvm.nix`.
  - **Not done, L4 (`HOME` equals the state dir):** a container run has no isolation from the state dir anyway, and a bind-mounted volume would lack a `home/` subdirectory, breaking agent CLIs.
