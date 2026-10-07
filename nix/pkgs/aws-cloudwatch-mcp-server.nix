# AWS CloudWatch MCP server (logs, metrics, alarms), pinned to the last
# release on mcp 1.x: nixpkgs ships mcp 1.29; 0.2+ needs mcp 2.x.
{ python3Packages, lib }:

python3Packages.buildPythonApplication rec {
  pname = "awslabs.cloudwatch-mcp-server";
  version = "0.1.8";
  pyproject = true;
  src = python3Packages.fetchPypi {
    pname = "awslabs_cloudwatch_mcp_server";
    inherit version;
    hash = "sha256-TMLOoGHe3FooJSALi78Q6ueFeuZ7Ngc3PCK23/WTsMw=";
  };
  build-system = [ python3Packages.hatchling ];
  dependencies = with python3Packages; [
    boto3
    loguru
    mcp
    numpy
    pandas
    pydantic
    requests
    statsmodels
  ];
  # Upstream tests call AWS; checks.aws-mcp-smoke covers start-up instead.
  doCheck = false;
  pythonImportsCheck = [ "awslabs.cloudwatch_mcp_server" ];
  meta = {
    description = "AWS CloudWatch MCP server";
    homepage = "https://github.com/awslabs/mcp";
    license = lib.licenses.asl20;
    mainProgram = "awslabs.cloudwatch-mcp-server";
  };
}
