# Microsoft's local Azure DevOps MCP server (@azure-devops/mcp), pinned, behind
# a wrapper that takes the org and PAT from env (the PAT never reaches argv).
{ buildNpmPackage, fetchFromGitHub, writeShellApplication, coreutils, lib, pkg-config, python3, libsecret, nodejs }:

let
  server = buildNpmPackage rec {
    pname = "azure-devops-mcp-server";
    version = "2.10.0";
    src = fetchFromGitHub {
      owner = "microsoft";
      repo = "azure-devops-mcp";
      tag = "v${version}";
      hash = "sha256-So/zsCt8uLAOjdk0OdgB6ENe9x2mmMy8YM4V/xlG+6c=";
    };
    npmDepsHash = "sha256-ZWWiPIVovkSDJNhih/neXuUlDJcMK21xk1P4j6uGNYY=";
    # Upstream tests are off; checks.azure-devops-mcp-smoke covers start-up.
    # keytar is a native addon that upstream loads at start-up, even with -a pat.
    nativeBuildInputs = [ pkg-config python3 ];
    buildInputs = [ libsecret ];
    env.npm_config_nodedir = nodejs;
    doCheck = false;
    meta = {
      description = "Azure DevOps MCP server (local, PAT)";
      homepage = "https://github.com/microsoft/azure-devops-mcp";
      license = lib.licenses.mit;
      mainProgram = "mcp-server-azuredevops";
    };
  };
in
writeShellApplication {
  name = "siphon-azure-devops-mcp";
  runtimeInputs = [ coreutils ];
  text = ''
    : "''${AZURE_DEVOPS_ORG:?AZURE_DEVOPS_ORG is required (the Azure DevOps organization name)}"
    : "''${AZURE_DEVOPS_PAT:?AZURE_DEVOPS_PAT is required (a personal access token)}"
    # printf is a builtin: the PAT is never in argv.
    PERSONAL_ACCESS_TOKEN=$(printf 'siphon:%s' "$AZURE_DEVOPS_PAT" | base64 -w0)
    export PERSONAL_ACCESS_TOKEN
    unset AZURE_DEVOPS_PAT
    # upstream's search, wiki and some write tools use Node's fetch, which ignores HTTPS_PROXY without this
    export NODE_USE_ENV_PROXY=1
    exec ${lib.getExe server} "$AZURE_DEVOPS_ORG" -a pat "$@"
  '';
  meta.mainProgram = "siphon-azure-devops-mcp";
}
