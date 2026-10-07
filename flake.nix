{
  description = "Siphon: draws events in from MCP servers and APIs, jets agents out (sources -> rules -> actions)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
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
        agentgw = siphon; # legacy-name
        # AWS MCP servers for services.siphon.aws (pinned; see nix/pkgs).
        aws-cloudwatch-mcp-server = pkgs.callPackage ./nix/pkgs/aws-cloudwatch-mcp-server.nix { };
        aws-documentation-mcp-server = pkgs.callPackage ./nix/pkgs/aws-documentation-mcp-server.nix { };
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
        # The old services.agentgw option path still evaluates to siphon. # legacy-name
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
