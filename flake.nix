{
  description = "Siphon: draws events in from MCP servers and APIs, jets agents out (sources -> rules -> actions)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.microvm = {
    url = "github:microvm-nix/microvm.nix";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      microvm,
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      microvmFor = import ./nix/microvm.nix { inherit self microvm nixpkgs; };
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      packages = forAll (pkgs: rec {
        siphon = pkgs.buildGoModule {
          pname = "siphon";
          version = self.shortRev or "dev";
          src = self;
          vendorHash = "sha256-Rz0KK7Pz4m1NcpoFn7tQfdi9qaEdnWoAZmBTQz5+Pyw=";
          env.CGO_ENABLED = 0;
          subPackages = [ "cmd/siphon" ];
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${self.shortRev or "dev"}"
          ];
          # Old binary name until v0.2.0. # legacy-name
          postInstall = "ln -s siphon $out/bin/agentgw"; # legacy-name
          meta.mainProgram = "siphon";
        };
        default = siphon;
        # Siphon in a microVM (full sandbox, host untouched): nix run .#microvm
        microvm = (microvmFor { system = pkgs.stdenv.hostPlatform.system; }).run;
        microvm-unfree =
          (microvmFor {
            system = pkgs.stdenv.hostPlatform.system;
            allowUnfreeAgents = true;
          }).run;
        # OCI image (stream: `nix build .#image && ./result | podman load`).
        image = import ./nix/image.nix { inherit pkgs siphon; };
        agentgw = siphon; # legacy-name
      });

      # Build your own image with agent CLIs: lib.<system>.mkImage { agentPackages = [ ... ]; }
      lib = forAll (pkgs: {
        mkImage =
          args:
          import ./nix/image.nix (
            {
              inherit pkgs;
              siphon = self.packages.${pkgs.stdenv.hostPlatform.system}.siphon;
            }
            // args
          );
      });

      nixosModules.default = import ./nix/module.nix self;

      checks = forAll (pkgs: {
        vm = import ./nix/vm-test.nix { inherit self pkgs; };
        # The old services.agentgw option path still evaluates to siphon. # legacy-name
        # configFile replaces the generated config in the unit.
        config-file =
          let
            sys = nixpkgs.lib.nixosSystem {
              inherit (pkgs.stdenv.hostPlatform) system;
              modules = [
                self.nixosModules.default
                {
                  boot.loader.grub.enable = false;
                  fileSystems."/" = {
                    device = "none";
                    fsType = "tmpfs";
                  };
                  system.stateVersion = "26.05";
                  services.siphon.enable = true;
                  services.siphon.configFile = "/etc/siphon/siphon.yaml";
                }
              ];
            };
          in
          assert nixpkgs.lib.hasInfix "-config /etc/siphon/siphon.yaml" sys.config.systemd.services.siphon.serviceConfig.ExecStart;
          pkgs.runCommand "config-file-ok" { } "touch $out";
        legacy-option =
          let
            sys = nixpkgs.lib.nixosSystem {
              inherit (pkgs.stdenv.hostPlatform) system;
              modules = [
                self.nixosModules.default
                {
                  boot.loader.grub.enable = false;
                  fileSystems."/" = {
                    device = "none";
                    fsType = "tmpfs";
                  };
                  system.stateVersion = "26.05";
                  services.agentgw.enable = true; # legacy-name
                }
              ];
            };
            warned = builtins.any (w: nixpkgs.lib.hasInfix "services.agentgw" w) sys.config.warnings; # legacy-name
          in
          assert sys.config.systemd.services ? siphon;
          assert warned;
          pkgs.runCommand "legacy-option-ok" { } "touch $out";
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            sqlite
          ];
        };
      });
    };
}
