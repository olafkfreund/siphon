# Siphon in a microVM: the full NixOS module (template-unit sandbox, egress
# restriction, polkit) without changing the host. Run with
# `nix run .#microvm`; see the runner below for the directory layout.
#
# Host directory ($SIPHON_VM_DIR, default ~/.config/siphon-vm):
#   config/   shared read-only-ish (9p) at /etc/siphon: siphon.yaml + secrets
#   state.img 2 GiB ext4 volume at /var/lib (DB, logins, portal edits)
# The portal is forwarded to http://127.0.0.1:8090.
{
  self,
  microvm,
  nixpkgs,
}:
{
  system,
  allowUnfreeAgents ? false,
}:
let
  vm = nixpkgs.lib.nixosSystem {
    inherit system;
    modules = [
      microvm.nixosModules.microvm
      self.nixosModules.default
      (
        { pkgs, lib, ... }:
        {
          networking.hostName = "siphon-vm";
          system.stateVersion = "26.05";
          # claude-code is unfree: only in the local -unfree variant.
          nixpkgs.config.allowUnfreePredicate = p: allowUnfreeAgents && lib.getName p == "claude-code";

          microvm = {
            hypervisor = "qemu";
            # q35, not the minimal "microvm" machine: the latter hangs in early
            # kernel boot on some AMD hosts (seen on a Threadripper 3995WX).
            qemu.machine = "q35";
            vcpu = 2;
            mem = 2048;
            # User (SLiRP) networking: no root, no bridge, no host changes.
            # The host is 10.0.2.2 from inside (e.g. a host Ollama).
            interfaces = [
              {
                type = "user";
                id = "usernet";
                mac = "02:00:00:51:70:01";
              }
            ];
            forwardPorts = [
              {
                from = "host";
                host.address = "127.0.0.1";
                host.port = 8090;
                guest.port = 8080;
              }
            ];
            # Relative paths resolve against the runner's working directory,
            # which the wrapper sets to $SIPHON_VM_DIR.
            shares = [
              {
                proto = "9p";
                tag = "siphon-config";
                source = "config";
                mountPoint = "/etc/siphon";
              }
            ];
            volumes = [
              {
                image = "state.img";
                mountPoint = "/var/lib";
                size = 2048;
              }
            ];
          };

          networking.firewall.allowedTCPPorts = [ 8080 ];
          services.siphon = {
            enable = true;
            configFile = "/etc/siphon/siphon.yaml";
            agentPackages = [ pkgs.codex ] ++ lib.optional allowUnfreeAgents pkgs.claude-code;
          };
          # Reachable through the forwarded port; the token protects it.
          systemd.services.siphon.environment.SIPHON_LISTEN = "0.0.0.0:8080";
          systemd.services.siphon.unitConfig.RequiresMountsFor = [
            "/etc/siphon"
            "/var/lib"
          ];
        }
      )
    ];
  };
  pkgs = nixpkgs.legacyPackages.${system};
  runner = vm.config.microvm.declaredRunner;
in
{
  inherit vm;
  # The wrapper: prepare the directory on first run, explain, then boot.
  run = pkgs.writeShellApplication {
    name = "siphon-microvm";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.openssl
    ];
    text = ''
      dir=''${SIPHON_VM_DIR:-$HOME/.config/siphon-vm}
      mkdir -p "$dir/config"
      chmod 700 "$dir"         # other host users stay out
      chmod 755 "$dir/config"  # the siphon user inside the VM reads it (9p)
      if [ ! -e "$dir/config/siphon.yaml" ]; then
        openssl rand -hex 24 | tr -d '\n' > "$dir/config/token"
        openssl rand -hex 16 | tr -d '\n' > "$dir/config/hook"
        cat > "$dir/config/siphon.yaml" <<'YAML'
      # Siphon inside the microVM (full sandbox). Edit freely; restart the VM to apply.
      server:
        db: /var/lib/siphon/state.db
        actions_dir: /var/lib/siphon-actions
        token: file:/etc/siphon/token
        egress: { socket: /run/siphon/egress.sock }
        # A host Ollama is 10.0.2.2 from inside the VM:
        # models: { private_endpoints: ["10.0.2.2:11434"] }

      sources:
        test:
          type: webhook
          secret: file:/etc/siphon/hook
          signature: github

      rules:
        - name: echo
          source: test
          when: 'event.kind == "echo"'
          on: each
          id: event.msg
          action: { cmd: [echo, "got: {{.event.msg}}"] }
      YAML
        # Files are read by the siphon user inside the VM; the 0700 directory
        # keeps other host users out.
        chmod 644 "$dir/config/"*
        echo "Created $dir/config (siphon.yaml, token, hook)."
      fi
      if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
        echo "siphon-microvm needs KVM: /dev/kvm must be readable and writable by $(id -un)." >&2
        exit 1
      fi
      echo "Siphon microVM: portal http://127.0.0.1:8090 (token: $dir/config/token)"
      echo "Config: $dir/config/siphon.yaml   State: $dir/state.img   Stop: Ctrl-C"
      cd "$dir"
      exec ${runner}/bin/microvm-run
    '';
  };
}
