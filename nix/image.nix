# The Siphon OCI image: the binary, CA certificates and time zones. No shell,
# no agent CLIs (claude-code is unfree; build your own variant with
# lib.<system>.mkImage { agentPackages = [ ... ]; }).
#
# Inside a container there is no systemd sandbox: Siphon runs with
# sandbox: none, says so at start, and shows an "Unsandboxed" banner. Use
# the NixOS module or the microVM for isolation.
{
  pkgs,
  siphon,
  agentPackages ? [ ],
  tag ? siphon.version,
}:

pkgs.dockerTools.streamLayeredImage {
  name = "ghcr.io/olafkfreund/siphon";
  inherit tag;
  contents = [
    pkgs.cacert
    pkgs.tzdata
  ]
  ++ agentPackages;
  # State volume owned by the non-root user; a writable /tmp for agents.
  extraCommands = ''
    mkdir -p var/lib/siphon etc/siphon tmp
    chmod 1777 tmp
  '';
  fakeRootCommands = ''
    chown -R 65532:65532 var/lib/siphon
  '';
  config = {
    User = "65532:65532";
    Entrypoint = [ "${siphon}/bin/siphon" ];
    Cmd = [
      "serve"
      "-config"
      "/etc/siphon/siphon.yaml"
    ];
    WorkingDir = "/var/lib/siphon";
    Volumes."/var/lib/siphon" = { };
    ExposedPorts."8080/tcp" = { };
    Env = [
      "SIPHON_LISTEN=0.0.0.0:8080"
      "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
      "TZDIR=${pkgs.tzdata}/share/zoneinfo"
      "HOME=/var/lib/siphon"
      "container=oci"
      "PATH=${pkgs.lib.makeBinPath agentPackages}"
    ];
    Labels = {
      "org.opencontainers.image.title" = "Siphon";
      "org.opencontainers.image.description" =
        "Draws events in from MCP servers and APIs, jets agents out";
      "org.opencontainers.image.source" = "https://github.com/olafkfreund/siphon";
      "org.opencontainers.image.licenses" = "Apache-2.0";
      "org.opencontainers.image.version" = siphon.version;
    };
  };
}
