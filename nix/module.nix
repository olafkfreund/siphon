self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.siphon;
  yaml = pkgs.formats.yaml { };
  stateDir = "/var/lib/siphon";
  # Shared with siphon-action@ instances through group siphon-io (setgid).
  actionsDir = "/var/lib/siphon-actions";
  # The egress proxy's unix socket; bound into restricted action units, whose
  # own network namespace has no route to the host (setgid: group siphon-io).
  egressDir = "/run/siphon";
  # server.db, actions_dir and egress.socket default here; everything else comes from settings.
  settings = lib.recursiveUpdate (lib.recursiveUpdate
    {
      server.db = "${stateDir}/state.db";
      server.actions_dir = actionsDir;
    }
    (lib.recursiveUpdate (lib.optionalAttrs cfg.egress.enable { server.egress.socket = "${egressDir}/egress.sock"; })
      (lib.recursiveUpdate
        (lib.optionalAttrs (cfg.models.privateEndpoints != [ ]) {
          server.models.private_endpoints = cfg.models.privateEndpoints;
        })
        (lib.optionalAttrs (cfg.mcpPackages != { }) {
          # Nix-pinned MCP servers the portal may enable by name (never a free command).
          server.mcp_packages = lib.mapAttrs (_: p: {
            command = [ (lib.getExe p.package) ] ++ p.args;
            inherit (p) env hosts;
          }) cfg.mcpPackages;
        })
      )
    )
  ) cfg.settings;
  configFile = yaml.generate "siphon.yaml" settings;
  units = settings.units or [ ];
  # What siphon sets in an AWS bridge's env (the package must allow each).
  awsEnv = [
    "AWS_ACCESS_KEY_ID"
    "AWS_SECRET_ACCESS_KEY"
    "AWS_SESSION_TOKEN"
    "AWS_REGION"
    "AWS_DEFAULT_REGION"
    "AWS_EC2_METADATA_DISABLED"
    "AWS_CONFIG_FILE"
    "AWS_SHARED_CREDENTIALS_FILE"
  ];
  metadataDeny = [
    "169.254.0.0/16"
    "fd00:ec2::254/128"
  ];
  actionServiceConfig = {
    Type = "exec";
    ExecStart = "${cfg.package}/bin/siphon exec-job ${actionsDir}/%i";
    DynamicUser = true;
    SupplementaryGroups = [ "siphon-io" ];
    ReadWritePaths = [ actionsDir ];
    UMask = "0027";
    RuntimeMaxSec = cfg.maxActionRuntime;
    TimeoutStopSec = "20s"; # bounds siphon's blocking stop of orphans
    LimitFSIZE = "16M"; # caps stdout/stderr files (and anything else it writes)
    TasksMax = 256;
    # System-wide agent CLI config must not reach agents: managed hooks,
    # instructions or extra MCP servers there would bypass the allowlist.
    # NixOS-managed dirs get an empty read-only tmpfs (the CLI sees no
    # config, rather than an unreadable one); others are made inaccessible.
    TemporaryFileSystem = map (d: "/etc/${d}:ro") (lib.filter etcManaged agentEtcDirs);
    InaccessiblePaths = map (d: "-/etc/${d}") (lib.filter (d: !etcManaged d) agentEtcDirs) ++ [
      # Unit names (and so other runs' ids and run dirs) are visible in
      # /run/systemd and over the system bus; neither is needed by a run.
      # stdout/stderr reach the journal through fds set up before this.
      "-/run/systemd"
      "-/run/dbus"
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
  # Network settings of the restricted templates (siphon-action@, siphon-mcp@).
  restrictedNet =
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
        { IPAddressDeny = metadataDeny; };
  # The sandboxed action unit. network is the only difference between the
  # restricted and the open template.
  actionUnit = network: {
    description = "siphon sandboxed action %i";
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
  # One-time move of an old install's state (copy, never move: rollback-safe).
  migrateLegacyState = pkgs.writeShellScript "siphon-migrate-state" ''
    set -eu
    old=/var/lib/agentgw # legacy-name
    new=${stateDir}
    # Runs once: never after it completed (an admin may reset state later),
    # and again if a previous copy was interrupted (.migrating left behind).
    [ -e "$new/MIGRATED_FROM_AGENTGW" ] && exit 0 # legacy-name
    if [ -e "$old/state.db" ] && { [ ! -e "$new/state.db" ] || [ -e "$new/.migrating" ]; }; then
      ${pkgs.coreutils}/bin/touch "$new/.migrating"
      ${pkgs.coreutils}/bin/cp -a "$old/." "$new/"
      ${pkgs.coreutils}/bin/touch "$new/MIGRATED_FROM_AGENTGW" # legacy-name
      ${pkgs.coreutils}/bin/chown -R siphon:siphon "$new"
      ${pkgs.coreutils}/bin/rm "$new/.migrating"
      echo "siphon: migrated state from $old to $new (old copy kept)"
    fi
  '';
in
{
  # Renamed from agentgw; old option paths keep working until v0.2.0. # legacy-name
  imports = [ (lib.mkRenamedOptionModule [ "services" "agentgw" ] [ "services" "siphon" ]) ]; # legacy-name

  options.services.siphon = {
    enable = lib.mkEnableOption "siphon, a gateway from MCP/API sources through rules to actions";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "siphon.packages.\${system}.default";
      description = "The siphon package.";
    };

    settings = lib.mkOption {
      type = yaml.type;
      default = { };
      description = ''
        siphon.yaml contents. Never put secrets here: the file is world-readable
        in the Nix store. Reference them as `env:NAME` (via environmentFile) or
        `file:/run/credentials/siphon.service/<name>` (via credentials).
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
        token = "/run/agenix/siphon-token";
      };
      description = "Secrets passed with LoadCredential; read them as file:/run/credentials/siphon.service/<name>.";
    };

    maxActionRuntime = lib.mkOption {
      type = lib.types.str;
      default = "2h";
      description = "Hard ceiling (RuntimeMaxSec) for any sandboxed action; per-action timeouts apply below it.";
    };

    mcpPackages = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            package = lib.mkOption {
              type = lib.types.package;
              description = "The MCP server package (its mainProgram is run).";
            };
            args = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ "stdio" ];
              description = "Arguments after the program.";
            };
            env = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              description = "Environment variable names a source may set (values are secret refs).";
            };
            hosts = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              description = "Hosts agents using this server may reach (port 443).";
            };
          };
        }
      );
      default = {
        github = {
          package = pkgs.github-mcp-server;
          env = [ "GITHUB_PERSONAL_ACCESS_TOKEN" ];
          hosts = [ "api.github.com" ];
        };
      };
      description = ''
        MCP servers sources may run by name (`package: <name>`), pinned by
        Nix. The portal can enable only these; any other command stays
        file-only.
      '';
    };

    aws.enable = lib.mkEnableOption ''
      the pinned AWS MCP servers (aws-cloudwatch, aws-docs) as mcpPackages.
      Off by default: the CloudWatch server pulls in pandas, numpy and
      statsmodels (about 1 GB)'';

    aws.configFile = lib.mkOption {
      # str, not path: the AWS config may name SSO accounts; keep it out of the store.
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/etc/siphon/aws-config";
      description = ''
        AWS config file (profiles, SSO, credential_process, role_arn chains)
        for credentials with `provider: aws` and `profile:`. Set as
        AWS_CONFIG_FILE for the siphon service; the SSO cache stays in the
        siphon user's home (run `sudo -u siphon aws sso login --profile …`).
      '';
    };

    models.privateEndpoints = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = lib.optional config.services.ollama.enable "127.0.0.1:${toString config.services.ollama.port}";
      defaultText = lib.literalExpression ''lib.optional config.services.ollama.enable "127.0.0.1:''${toString config.services.ollama.port}"'';
      example = [ "192.168.1.20:11434" ];
      description = ''
        Private (loopback/LAN) model endpoints agents may reach, as exact
        host:port. A local services.ollama is included automatically.
        Link-local and cloud metadata addresses are never reachable.
      '';
    };

    egress.enable = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Restrict sandboxed agent runs (and cmd actions that opt in) to
        siphon's egress proxy, which allows only each run's host allowlist.
        Runs without egress use the open siphon-action-open@ template.
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
    warnings = lib.optional (cfg.aws.enable && !(cfg.mcpPackages ? aws-cloudwatch))
      "services.siphon.aws.enable: mcpPackages is set explicitly, so the AWS servers were not added; add aws-cloudwatch and aws-docs to it";
    # mkOptionDefault: merge with the default (github) instead of replacing it.
    services.siphon.mcpPackages = lib.mkIf cfg.aws.enable (lib.mkOptionDefault {
      aws-cloudwatch = {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.aws-cloudwatch-mcp-server;
        args = [ ]; # stdio by default
        env = awsEnv;
        hosts = [
          "logs.{region}.amazonaws.com"
          "monitoring.{region}.amazonaws.com"
        ];
      };
      aws-docs = {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.aws-documentation-mcp-server;
        args = [ ];
        env = [ "FASTMCP_LOG_LEVEL" ];
        hosts = [
          "docs.aws.amazon.com"
          "proxy.search.docs.aws.com"
          "api.contentrecs.docs.aws.com"
        ];
      };
    });

    assertions =
      let
        secretPaths = lib.attrValues cfg.credentials ++ lib.optional (cfg.environmentFile != null) cfg.environmentFile;
      in
      map (p: {
        assertion = lib.hasPrefix "/" p && !lib.hasPrefix builtins.storeDir p;
        message = "services.siphon: secret path ${p} must be an absolute path outside the Nix store";
      }) secretPaths;

    users.users.siphon = {
      isSystemUser = true;
      group = "siphon";
      extraGroups = [ "siphon-io" ];
    };
    users.groups.siphon = { };
    users.groups.siphon-io = { };

    # setgid: run dirs and files inherit group siphon-io; group may traverse
    # but not list, so one action cannot enumerate other runs.
    systemd.tmpfiles.rules = [
      "d ${actionsDir} 2710 siphon siphon-io -"
      "d ${egressDir} 2710 siphon siphon-io -"
    ];

    systemd.services.siphon = {
      description = "siphon gateway";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      path = [ config.systemd.package ]; # systemctl, to start siphon-action@ instances
      environment = lib.optionalAttrs (cfg.aws.configFile != null) { AWS_CONFIG_FILE = cfg.aws.configFile; };
      serviceConfig = {
        ExecStartPre = [
          "+${migrateLegacyState}"
          # -file-only: a bad portal edit must not stop the service; serve falls
          # back to the last valid revision and shows a banner instead.
          "${cfg.package}/bin/siphon validate -file-only -config ${configFile}"
        ];
        ExecStart = "${cfg.package}/bin/siphon serve -config ${configFile}";
        User = "siphon";
        Group = "siphon";
        StateDirectory = "siphon";
        StateDirectoryMode = "0700";
        ReadWritePaths = [
          actionsDir
          egressDir
        ];
        # Explicit: the setgid hand-off breaks silently without this group
        # (a non-member's chmod drops the setgid bit).
        SupplementaryGroups = [ "siphon-io" ];
        WorkingDirectory = stateDir;
        EnvironmentFile = lib.mkIf (cfg.environmentFile != null) cfg.environmentFile;
        LoadCredential = lib.mapAttrsToList (n: p: "${n}:${p}") cfg.credentials;
        Restart = "on-failure";
        # Never reach the cloud metadata service, whatever a portal edit sets (allow_private).
        IPAddressDeny = metadataDeny;
        # Hardening. Actions run in siphon-action@ instances (below), started
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
    # their hardening is fixed here, so siphon can't loosen it. %i is a
    # 16-hex run id. PID 1 opens nothing in siphon-writable directories:
    # exec-job, as the DynamicUser, reads job.json and creates stdout/stderr.
    # Restricted: the only reachable address is siphon's egress proxy, so an
    # agent that ignores HTTPS_PROXY gets no network at all.
    systemd.services."siphon-action@" = actionUnit restrictedNet;
    # MCP bridge: a stdio MCP server that holds secrets runs here, under its
    # own DynamicUser, never in the agent's unit. Always the restricted
    # network; it reaches only its package's hosts through the egress proxy.
    systemd.services."siphon-mcp@" =
      actionUnit (
        restrictedNet
        // {
          # The MCP server's secrets: written 0600 by siphon into its 0700
          # state dir (never in the group-shared run dir), read by PID 1 and
          # handed only to this instance as $CREDENTIALS_DIRECTORY/bridge.
          LoadCredential = [ "bridge:${stateDir}/bridge-secrets/%i" ];
        }
      )
      // {
        description = "siphon MCP bridge %i";
      };
    # Open: for runs with egress off (cmd actions by default).
    systemd.services."siphon-action-open@" = actionUnit { IPAddressDeny = metadataDeny; };

    # siphon may only start/stop/reset its own template instances and start
    # the units in settings.units. No transient units: their properties are
    # invisible to polkit, which made the account root-equivalent before.
    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
        if (subject.user != "siphon") {
          return polkit.Result.NOT_HANDLED;
        }
        if (action.id == "org.freedesktop.systemd1.manage-units") {
          var unit = action.lookup("unit") || "";
          var verb = action.lookup("verb") || "";
          var allowed = ${builtins.toJSON units};
          if (/^siphon-(action(-open)?|mcp)@[0-9a-f]{16}\.service$/.test(unit) &&
              (verb == "start" || verb == "stop" || verb == "reset-failed")) {
            return polkit.Result.YES;
          }
          if (allowed.indexOf(unit) >= 0 && verb == "start") {
            return polkit.Result.YES;
          }
        }
        // Everything else is refused for siphon (this rule must not be
        // preceded by a broader rule granting it).
        return polkit.Result.NO;
      });
    '';
  };
}
