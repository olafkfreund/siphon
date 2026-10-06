{ pkgs, ... }:

{
  # https://devenv.sh/languages/
  languages.go.enable = true;

  # https://devenv.sh/packages/ — what the tests, docs and VM test use.
  packages = [
    pkgs.gopls
    pkgs.sqlite
    pkgs.jq
    pkgs.curl
    pkgs.openssl
  ];

  # Keep Go from downloading a toolchain behind our back.
  env.GOTOOLCHAIN = "local";

  # https://devenv.sh/scripts/
  scripts = {
    run-tests.exec = "go vet ./... && go test -race ./...";
    schema.exec = ''UPDATE_SCHEMA=1 go test ./internal/config/ -run TestSchemaUpToDate && echo "schema regenerated"'';
    vm-test.exec = "nix build .#checks.x86_64-linux.vm -L";
    agentgw.exec = ''go run ./cmd/agentgw "$@"'';
  };

  enterShell = ''
    echo "agentgw dev shell: $(go version | cut -d' ' -f3)"
    echo "  run-tests | schema | vm-test | agentgw <cmd>"
  '';

  # https://devenv.sh/tests/ — `devenv test` runs this before devenv:enterTest.
  tasks."agentgw:test" = {
    exec = "go vet ./... && go test -race ./...";
    before = [ "devenv:enterTest" ];
  };
}
