self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.agentgw;
  yaml = pkgs.formats.yaml { };
  stateDir = "/var/lib/agentgw";
  # server.db defaults into StateDirectory; everything else comes from settings.
  settings = lib.recursiveUpdate { server.db = "${stateDir}/state.db"; } cfg.settings;
  configFile = yaml.generate "agentgw.yaml" settings;
  units = settings.units or [ ];
in
{
  options.services.agentgw = {
    enable = lib.mkEnableOption "agentgw, a gateway from MCP/API sources through rules to actions";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "agentgw.packages.\${system}.default";
      description = "The agentgw package.";
    };

    settings = lib.mkOption {
      type = yaml.type;
      default = { };
      description = ''
        agentgw.yaml contents. Never put secrets here: the file is world-readable
        in the Nix store. Reference them as `env:NAME` (via environmentFile) or
        `file:/run/credentials/agentgw.service/<name>` (via credentials).
      '';
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Optional EnvironmentFile with secrets referenced as env:NAME.";
    };

    credentials = lib.mkOption {
      type = lib.types.attrsOf lib.types.path;
      default = { };
      example = {
        token = "/run/agenix/agentgw-token";
      };
      description = "Secrets passed with LoadCredential; read them as file:/run/credentials/agentgw.service/<name>.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.agentgw = {
      isSystemUser = true;
      group = "agentgw";
    };
    users.groups.agentgw = { };

    systemd.services.agentgw = {
      description = "agentgw gateway";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      path = [ config.systemd.package ]; # systemd-run and systemctl for actions
      serviceConfig = {
        ExecStartPre = "${cfg.package}/bin/agentgw validate -config ${configFile}";
        ExecStart = "${cfg.package}/bin/agentgw serve -config ${configFile}";
        User = "agentgw";
        Group = "agentgw";
        StateDirectory = "agentgw";
        StateDirectoryMode = "0700";
        WorkingDirectory = stateDir;
        EnvironmentFile = lib.mkIf (cfg.environmentFile != null) cfg.environmentFile;
        LoadCredential = lib.mapAttrsToList (n: p: "${n}:${p}") cfg.credentials;
        Restart = "on-failure";
        # Hardening. Actions run in separate transient units with their own
        # sandbox (DynamicUser etc.), started over D-Bus and authorised by
        # the polkit rule below, so the gateway itself needs no privileges.
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictNamespaces = true;
        LockPersonality = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        SystemCallArchitectures = "native";
        CapabilityBoundingSet = "";
        UMask = "0077";
      };
    };

    # agentgw may start only its own transient units (agentgw-run-*, agentgw-agent-*)
    # and the units listed in settings.units.
    # ponytail: polkit cannot inspect transient-unit properties, so it cannot stop
    # agentgw from asking for User=root. Treat control of the agentgw account as
    # root-equivalent; see the security notes in README.md.
    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
        if (subject.user != "agentgw" || action.id != "org.freedesktop.systemd1.manage-units") {
          return polkit.Result.NOT_HANDLED;
        }
        var unit = action.lookup("unit") || "";
        var verb = action.lookup("verb") || "";
        var allowed = ${builtins.toJSON units};
        if (/^agentgw-(run|agent)-[0-9a-f]+\.service$/.test(unit) &&
            (verb == "start" || verb == "stop")) {
          return polkit.Result.YES;
        }
        if (allowed.indexOf(unit) >= 0 && verb == "start") {
          return polkit.Result.YES;
        }
        return polkit.Result.NO;
      });
    '';
  };
}
