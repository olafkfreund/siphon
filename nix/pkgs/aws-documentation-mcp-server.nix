# AWS documentation MCP server (search and read docs.aws.amazon.com), pinned
# to the last release on mcp 1.x (nixpkgs ships mcp 1.29).
{ python3Packages, lib }:

python3Packages.buildPythonApplication rec {
  pname = "awslabs.aws-documentation-mcp-server";
  version = "1.1.30";
  pyproject = true;
  src = python3Packages.fetchPypi {
    pname = "awslabs_aws_documentation_mcp_server";
    inherit version;
    hash = "sha256-8znWDmbPFEPTJ5THig1BmljoK9tQgPQ1o0BU6A1pMWc=";
  };
  build-system = [ python3Packages.hatchling ];
  dependencies = with python3Packages; [
    beautifulsoup4
    httpx
    loguru
    markdownify
    mcp
    pydantic
  ];
  # Upstream tests fetch docs.aws.amazon.com; checks.aws-mcp-smoke covers start-up.
  doCheck = false;
  pythonImportsCheck = [ "awslabs.aws_documentation_mcp_server" ];
  meta = {
    description = "AWS documentation MCP server";
    homepage = "https://github.com/awslabs/mcp";
    license = lib.licenses.asl20;
    mainProgram = "awslabs.aws-documentation-mcp-server";
  };
}
