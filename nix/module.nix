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
      # str, not path: a path literal would copy the secret into the Nix store.
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Optional EnvironmentFile with secrets referenced as env:NAME.";
    };

    credentials = lib.mkOption {
      # str, not path: a path literal would copy the secret into the Nix store.
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = {
        token = "/run/agenix/agentgw-token";
      };
      description = "Secrets passed with LoadCredential; read them as file:/run/credentials/agentgw.service/<name>.";
    };

    maxActionRuntime = lib.mkOption {
      type = lib.types.str;
      default = "2h";
      description = "Hard ceiling (RuntimeMaxSec) for any sandboxed action; per-action timeouts apply below it.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions =
      let
        secretPaths = lib.attrValues cfg.credentials ++ lib.optional (cfg.environmentFile != null) cfg.environmentFile;
      in
      map (p: {
        assertion = lib.hasPrefix "/" p && !lib.hasPrefix builtins.storeDir p;
        message = "services.agentgw: secret path ${p} must be an absolute path outside the Nix store";
      }) secretPaths;

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
      path = [ config.systemd.package ]; # systemctl, to start agentgw-action@ instances
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
        # Hardening. Actions run in agentgw-action@ instances (below), started
        # over D-Bus and authorised by the polkit rule, so the gateway itself
        # needs no privileges.
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

    # Every sandboxed cmd/agent run is an instance of this template; its
    # hardening is fixed here, so agentgw can't loosen it. Instance %i is a
    # 16-hex run id; agentgw writes job.json and the output files beforehand.
    systemd.services."agentgw-action@" = {
      description = "agentgw sandboxed action %i";
      path = [ "/run/current-system/sw" ]; # tools actions may call (claude, etc.)
      serviceConfig = {
        Type = "exec";
        ExecStart = "${cfg.package}/bin/agentgw exec-job";
        LoadCredential = "job:${stateDir}/actions/%i/job.json";
        StandardOutput = "file:${stateDir}/actions/%i/stdout";
        StandardError = "file:${stateDir}/actions/%i/stderr";
        RuntimeMaxSec = cfg.maxActionRuntime;
        DynamicUser = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictNamespaces = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        CapabilityBoundingSet = "";
        IPAddressDeny = [ "169.254.0.0/16" "fd00:ec2::254/128" ];
        UMask = "0077";
      };
    };

    # agentgw may only start/stop/reset its own template instances and start
    # the units in settings.units. No transient units: their properties are
    # invisible to polkit, which made the account root-equivalent before.
    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
        if (subject.user != "agentgw" || action.id != "org.freedesktop.systemd1.manage-units") {
          return polkit.Result.NOT_HANDLED;
        }
        var unit = action.lookup("unit") || "";
        var verb = action.lookup("verb") || "";
        var allowed = ${builtins.toJSON units};
        if (/^agentgw-action@[0-9a-f]{16}\.service$/.test(unit) &&
            (verb == "start" || verb == "stop" || verb == "reset-failed")) {
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
