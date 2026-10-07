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
          vendorHash = "sha256-8s5VV3W8KjA3n/+ReK4Xph6FplXmEZJN9mC6HSYDmwI=";
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
        # AWS MCP servers for services.siphon.aws (pinned; see nix/pkgs).
        aws-cloudwatch-mcp-server = pkgs.callPackage ./nix/pkgs/aws-cloudwatch-mcp-server.nix { };
        aws-documentation-mcp-server = pkgs.callPackage ./nix/pkgs/aws-documentation-mcp-server.nix { };
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
        # Both pinned AWS MCP servers start over stdio and list tools, offline.
        aws-mcp-smoke = pkgs.runCommand "aws-mcp-smoke" { nativeBuildInputs = [ pkgs.python3 ]; } ''
          export HOME=$TMPDIR AWS_ACCESS_KEY_ID=AKIAFAKE AWS_SECRET_ACCESS_KEY=fake AWS_SESSION_TOKEN=fake
          export AWS_REGION=eu-west-1 AWS_EC2_METADATA_DISABLED=true FASTMCP_LOG_LEVEL=ERROR
          for bin in ${pkgs.lib.getExe self.packages.${pkgs.stdenv.hostPlatform.system}.aws-cloudwatch-mcp-server} \
                     ${pkgs.lib.getExe self.packages.${pkgs.stdenv.hostPlatform.system}.aws-documentation-mcp-server}; do
            python3 ${./nix/aws-mcp-smoke.py} "$bin"
          done
          touch $out
        '';
        # aws.enable adds both AWS servers next to github, with {region} hosts,
        # and aws.configFile reaches the service.
        aws-module =
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
                  services.siphon = {
                    enable = true;
                    aws.enable = true;
                    aws.configFile = "/etc/siphon/aws-config";
                  };
                }
              ];
            };
            pk = sys.config.services.siphon.mcpPackages;
          in
          assert pk ? github && pk ? aws-cloudwatch && pk ? aws-docs;
          assert builtins.elem "logs.{region}.amazonaws.com" pk.aws-cloudwatch.hosts;
          assert sys.config.systemd.services.siphon.environment.AWS_CONFIG_FILE == "/etc/siphon/aws-config";
          assert sys.config.warnings == [ ];
          pkgs.runCommand "aws-module-ok" { } "touch $out";
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
