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
  # Shared with agentgw-action@ instances through group agentgw-io (setgid).
  actionsDir = "/var/lib/agentgw-actions";
  # The egress proxy's unix socket; bound into restricted action units, whose
  # own network namespace has no route to the host (setgid: group agentgw-io).
  egressDir = "/run/agentgw";
  # server.db, actions_dir and egress.socket default here; everything else comes from settings.
  settings = lib.recursiveUpdate (lib.recursiveUpdate
    {
      server.db = "${stateDir}/state.db";
      server.actions_dir = actionsDir;
    }
    (lib.optionalAttrs cfg.egress.enable { server.egress.socket = "${egressDir}/egress.sock"; })
  ) cfg.settings;
  configFile = yaml.generate "agentgw.yaml" settings;
  units = settings.units or [ ];
  metadataDeny = [
    "169.254.0.0/16"
    "fd00:ec2::254/128"
  ];
  actionServiceConfig = {
    Type = "exec";
    ExecStart = "${cfg.package}/bin/agentgw exec-job ${actionsDir}/%i";
    DynamicUser = true;
    SupplementaryGroups = [ "agentgw-io" ];
    ReadWritePaths = [ actionsDir ];
    UMask = "0027";
    RuntimeMaxSec = cfg.maxActionRuntime;
    TimeoutStopSec = "20s"; # bounds agentgw's blocking stop of orphans
    LimitFSIZE = "16M"; # caps stdout/stderr files (and anything else it writes)
    TasksMax = 256;
    # System-wide agent CLI config must not reach agents: managed hooks,
    # instructions or extra MCP servers there would bypass the allowlist.
    # NixOS-managed dirs get an empty read-only tmpfs (the CLI sees no
    # config, rather than an unreadable one); others are made inaccessible.
    TemporaryFileSystem = map (d: "/etc/${d}:ro") (lib.filter etcManaged agentEtcDirs);
    InaccessiblePaths = map (d: "-/etc/${d}") (lib.filter (d: !etcManaged d) agentEtcDirs) ++ [
      # Unit names there reveal other runs' ids (and so their run dirs).
      "-/run/systemd/units"
      # The daemon fetches URLs for any client: network outside the sandbox.
      "-/nix/var/nix/daemon-socket"
    ];
    PrivateTmp = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    NoNewPrivileges = true;
    PrivateDevices = true;
    ProtectProc = "invisible";
    ProcSubset = "pid";
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    # Own cgroup namespace: other runs' ids (unit names) stay invisible.
    ProtectControlGroups = "private";
    ProtectClock = true;
    ProtectHostname = true;
    RestrictNamespaces = true;
    RestrictSUIDSGID = true;
    RestrictRealtime = true;
    LockPersonality = true;
    CapabilityBoundingSet = "";
    RestrictAddressFamilies = [
      "AF_UNIX"
      "AF_INET"
      "AF_INET6"
    ];
    SystemCallArchitectures = "native";
    SystemCallFilter = [ "@system-service" ];
  };
  # The sandboxed action unit. network is the only difference between the
  # restricted and the open template.
  actionUnit = network: {
    description = "agentgw sandboxed action %i";
    # Agent CLIs first, then the system profile for tools actions may call.
    path = cfg.agentPackages ++ [ "/run/current-system/sw" ];
    # mkMerge, not //: list options (InaccessiblePaths, ...) must concatenate.
    serviceConfig = lib.mkMerge [
      actionServiceConfig
      network
    ];
  };
  agentEtcDirs = [
    "claude-code"
    "codex"
    "gemini"
    "antigravity"
  ];
  etcManaged = d: lib.any (k: k == d || lib.hasPrefix "${d}/" k) (lib.attrNames config.environment.etc);
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

    egress.enable = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Restrict sandboxed agent runs (and cmd actions that opt in) to
        agentgw's egress proxy, which allows only each run's host allowlist.
        Runs without egress use the open agentgw-action-open@ template.
      '';
    };

    agentPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      example = lib.literalExpression "[ pkgs.claude-code pkgs.codex ]";
      description = "Agent CLIs (claude, codex, agy) made available to sandboxed agent runs.";
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
      extraGroups = [ "agentgw-io" ];
    };
    users.groups.agentgw = { };
    users.groups.agentgw-io = { };

    # setgid: run dirs and files inherit group agentgw-io; group may traverse
    # but not list, so one action cannot enumerate other runs.
    systemd.tmpfiles.rules = [
      "d ${actionsDir} 2710 agentgw agentgw-io -"
      "d ${egressDir} 2710 agentgw agentgw-io -"
    ];

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
        ReadWritePaths = [
          actionsDir
          egressDir
        ];
        # Explicit: the setgid hand-off breaks silently without this group
        # (a non-member's chmod drops the setgid bit).
        SupplementaryGroups = [ "agentgw-io" ];
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

    # Every sandboxed cmd/agent run is an instance of one of these templates;
    # their hardening is fixed here, so agentgw can't loosen it. %i is a
    # 16-hex run id. PID 1 opens nothing in agentgw-writable directories:
    # exec-job, as the DynamicUser, reads job.json and creates stdout/stderr.
    # Restricted: the only reachable address is agentgw's egress proxy, so an
    # agent that ignores HTTPS_PROXY gets no network at all.
    systemd.services."agentgw-action@" = actionUnit (
      if cfg.egress.enable then
        {
          # Own network namespace: only its lo, where exec-job forwards
          # 127.0.0.1:3128 to the proxy's unix socket. No host port is
          # reachable; the IP filter is a second layer.
          PrivateNetwork = true;
          IPAddressDeny = [ "any" ];
          IPAddressAllow = [ "127.0.0.1/32" ];
          # An empty /run: no host daemon socket (nscd, D-Bus, resolved,
          # avahi, tailscale, databases...) is reachable, only the proxy's.
          # connect() works on a read-only bind.
          TemporaryFileSystem = [ "/run:ro" ];
          BindReadOnlyPaths = [
            egressDir
            "/run/current-system" # PATH: /run/current-system/sw
          ];
        }
      else
        { IPAddressDeny = metadataDeny; }
    );
    # Open: for runs with egress off (cmd actions by default).
    systemd.services."agentgw-action-open@" = actionUnit { IPAddressDeny = metadataDeny; };

    # agentgw may only start/stop/reset its own template instances and start
    # the units in settings.units. No transient units: their properties are
    # invisible to polkit, which made the account root-equivalent before.
    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
        if (subject.user != "agentgw") {
          return polkit.Result.NOT_HANDLED;
        }
        if (action.id == "org.freedesktop.systemd1.manage-units") {
          var unit = action.lookup("unit") || "";
          var verb = action.lookup("verb") || "";
          var allowed = ${builtins.toJSON units};
          if (/^agentgw-action(-open)?@[0-9a-f]{16}\.service$/.test(unit) &&
              (verb == "start" || verb == "stop" || verb == "reset-failed")) {
            return polkit.Result.YES;
          }
          if (allowed.indexOf(unit) >= 0 && verb == "start") {
            return polkit.Result.YES;
          }
        }
        // Everything else is refused for agentgw (this rule must not be
        // preceded by a broader rule granting it).
        return polkit.Result.NO;
      });
    '';
  };
}
