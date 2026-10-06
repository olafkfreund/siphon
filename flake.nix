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
          vendorHash = "sha256-Rz0KK7Pz4m1NcpoFn7tQfdi9qaEdnWoAZmBTQz5+Pyw=";
          env.CGO_ENABLED = 0;
          subPackages = [ "cmd/agentgw" ];
          ldflags = [ "-s" "-w" "-X main.version=${self.shortRev or "dev"}" ];
        };
      });

      nixosModules.default = import ./nix/module.nix self;

      checks = forAll (pkgs: {
        vm = import ./nix/vm-test.nix { inherit self pkgs; };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = with pkgs; [ go gopls sqlite ]; };
      });
    };
}
