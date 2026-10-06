{
  description = "agentgw: self-hosted MCP/API gateway (sources -> rules -> actions)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "agentgw";
          version = self.shortRev or "dev";
          src = self;
          vendorHash = "sha256-WFuT25UpII2m2xRRHdYmDSJj+dzTDElT2XnPuk1kxUw=";
          env.CGO_ENABLED = 0;
          subPackages = [ "cmd/agentgw" ];
          ldflags = [ "-s" "-w" "-X main.version=${self.shortRev or "dev"}" ];
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = with pkgs; [ go gopls sqlite ]; };
      });
    };
}
