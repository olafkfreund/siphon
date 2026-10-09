{ self, pkgs }:

let
  # A stdio MCP server that needs a secret: run through the MCP bridge.
  stubMcp = pkgs.writeShellScriptBin "stub-mcp" "exec ${pkgs.python3}/bin/python3 ${./stub-mcp-stdio.py}";
  stubToken = "tok-123-SECRET-bridge";
  # The same stub, slow to answer, so a run can be inspected while it's live.
  stubMcpSlow = pkgs.writeShellScriptBin "stub-mcp" "STUB_SLOW=10 exec ${pkgs.python3}/bin/python3 ${./stub-mcp-stdio.py}";
  # The real agent CLIs (claude-code is unfree), run in the restricted
  # template to prove they start without nscd and reach the network only
  # through the forwarder.
  unfree = import pkgs.path {
    inherit (pkgs.stdenv.hostPlatform) system;
    config.allowUnfreePredicate = p: (p.pname or "") == "claude-code";
  };
  realClis = pkgs.writeShellScriptBin "real-cli-check" ''
    t=$(mktemp -d)
    ${unfree.claude-code}/bin/claude --version >/dev/null 2>&1; echo "claude-version-rc=$?"
    ${pkgs.codex}/bin/codex --version >/dev/null 2>&1; echo "codex-version-rc=$?"
    HOME=$t ANTHROPIC_API_KEY=sk-ant-dummy timeout 90 ${unfree.claude-code}/bin/claude --bare -p hi 2>&1 | tail -5
    HOME=$t CODEX_HOME=$t OPENAI_API_KEY=sk-dummy timeout 90 ${pkgs.codex}/bin/codex exec --skip-git-repo-check hi </dev/null 2>&1 | tail -5
    true
  '';
in
pkgs.testers.runNixOSTest {
  name = "siphon";

  # A host outside the sandbox: the only way to it from a restricted action
  # is siphon's egress proxy.
  nodes.external = {
    networking.firewall.allowedTCPPorts = [
      8080
      8000
    ];
    # A stand-in model endpoint (OpenAI-compatible) and MCP server.
    systemd.services.stub-model = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 ${./stub-model.py}";
    };
    systemd.services.web = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 -m http.server 8080 --directory ${pkgs.writeTextDir "index.html" "external-ok"}";
    };
  };

  nodes.machine =
    { pkgs, ... }:
    {
      imports = [ self.nixosModules.default ];

      # Test-only secrets; real deployments use agenix/sops paths.
      environment.etc."siphon/token".text = "test-token-0123456789abcdef0123456789";
      environment.etc."siphon/hook".text = "hook-secret";
      environment.etc."siphon/metrics-token".text = "scrape-token-0123456789abcdef0123456789";

      environment.etc."siphon/stub-token".text = stubToken;
      # A normal user for the CLI walkthrough (docs/getting-started.md).
      users.users.alice.isNormalUser = true;
      # AWS: a base key pair for AssumeRole against a fake STS on loopback.
      environment.etc."siphon/aws-id".text = "AKIABASEVMTEST";
      environment.etc."siphon/aws-secret".text = "base-secret-NEVER-vm";
      systemd.services.stub-sts = {
        wantedBy = [ "multi-user.target" ];
        before = [ "siphon.service" ];
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 ${./stub-sts.py}";
      };
      systemd.services.siphon.environment.AWS_ENDPOINT_URL_STS = "http://127.0.0.1:8099";
      # A webhook notification channel on loopback (docs/tasks/notifications.md).
      systemd.services.notify-recv = {
        wantedBy = [ "multi-user.target" ];
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 ${./notify-recv.py}";
      };
      environment.etc."siphon/gl-token".text = "gl-hook-token-123";
      # Standard Webhooks secret: whsec_ + base64 of the key bytes.
      environment.etc."siphon/std-secret".text = "whsec_c2lwaG9uLXN0YW5kYXJkLXdlYmhvb2tzLWtleQ==";
      services.siphon = {
        enable = true;
        backup.enable = true;
        mcpPackages.stubaws = {
          package = stubMcpSlow;
          args = [ ];
          env = [
            "AWS_ACCESS_KEY_ID"
            "AWS_SECRET_ACCESS_KEY"
            "AWS_SESSION_TOKEN"
            "AWS_REGION"
            "AWS_DEFAULT_REGION"
            "AWS_EC2_METADATA_DISABLED"
            "AWS_CONFIG_FILE"
            "AWS_SHARED_CREDENTIALS_FILE"
          ];
          hosts = [ "logs.{region}.amazonaws.com" ];
        };
        mcpPackages.stub = {
          package = stubMcp;
          args = [ ];
          env = [ "STUB_TOKEN" ];
        };
        credentials = {
          token = "/etc/siphon/token";
          hook = "/etc/siphon/hook";
          metrics-token = "/etc/siphon/metrics-token";
        };
        settings = {
          server = {
            # All addresses: the restricted sandbox must still not reach it.
            listen = "0.0.0.0:8080";
            token = "file:/run/credentials/siphon.service/token";
            metrics.token = "file:/run/credentials/siphon.service/metrics-token";
            # Where the browser returns after an OAuth login (#44).
            public_url = "http://127.0.0.1:8080";
            # SSO for the portal (#46). Nothing listens on :9, so discovery
            # fails; the token login (used by the subtests above) keeps working.
            oidc = {
              issuer = "http://127.0.0.1:9";
              client_id = "siphon";
              allow_private = true;
              roles.viewer.emails = [ "nobody@example.com" ];
            };
          };
          sources.gh = {
            type = "webhook";
            secret = "file:/run/credentials/siphon.service/hook";
            signature = "github";
          };
          # A stand-in agent runner: same template unit as a real `claude` run.
          agents.slow = {
            # Takes 15 s to honour SIGTERM, so overlapping copies would be visible.
            command = "slow-agent";
            prompt = "p";
            approve = false;
            # Own throwaway credential, so it never holds a subscription's slot.
            credential = "slow-key";
          };
          credentials = {
            slow-key = {
              provider = "claude";
              api_key = "file:/etc/siphon/token";
            };
            claude-max.provider = "claude";
            chatgpt.provider = "codex";
            # A model connection to the stub (a LAN endpoint, so it must be listed).
            stubm = {
              provider = "openai";
              url = "http://external:8000/v1";
            };
            google.provider = "agy";
            aws-test = {
              provider = "aws";
              region = "eu-west-1";
              role_arn = "arn:aws:iam::123456789012:role/siphon-test";
              external_id = "vm-ext";
              access_key_id = "file:/etc/siphon/aws-id";
              secret_access_key = "file:/etc/siphon/aws-secret";
            };
          };
          server.models.private_endpoints = [ "external:8000" ];
          server.services.private_endpoints = [ "127.0.0.1:18099" ];
          sources.extm = {
            type = "mcp";
            url = "http://external:8000/mcp";
            read = {
              tool = "echo";
              args.text = "poll";
            };
            poll = "1h";
            allow_private = true;
          };
          # Webhook auth modes: a GitLab-style header token and Standard Webhooks.
          sources.glhook = {
            type = "webhook";
            signature = "token";
            token_header = "X-Gitlab-Token";
            secret = "file:/etc/siphon/gl-token";
          };
          sources.stdhook = {
            type = "webhook";
            signature = "standard-webhooks";
            secret = "file:/etc/siphon/std-secret";
          };
          # An OAuth MCP source with no login and nothing listening (#44).
          sources.oauthsrc = {
            type = "mcp";
            url = "http://127.0.0.1:9/mcp";
            allow_private = true;
            auth.oauth = { };
          };
          # A stdio MCP server with a secret env: agents reach it via the bridge.
          sources.stubsrc = {
            type = "mcp";
            package = "stub";
            env.STUB_TOKEN = "file:/etc/siphon/stub-token";
            read.tool = "whoami";
            poll = "1h";
          };
          # An AWS source: short-lived keys from the fake STS reach only its bridge.
          sources.awssrc = {
            type = "mcp";
            package = "stubaws";
            aws = "aws-test";
          };
          agents.awsagent = {
            kind = "model";
            credential = "stubm";
            model = "stub-model";
            prompt = "Use the tool, then answer.";
            mcp = [ "awssrc" ];
            allowed_tools = [ "mcp__awssrc__whoami" ];
            max_turns = 3;
            approve = false;
          };
          agents.bridged = {
            kind = "model";
            credential = "stubm";
            model = "stub-model";
            prompt = "Use the tool, then answer.";
            mcp = [ "stubsrc" ];
            allowed_tools = [ "mcp__stubsrc__whoami" ];
            max_turns = 3;
            approve = false;
          };
          # The built-in loop on a model connection, with one allowed MCP tool.
          agents.local = {
            kind = "model";
            credential = "stubm";
            model = "stub-model";
            prompt = "Use the echo tool, then answer.";
            mcp = [ "extm" ];
            allowed_tools = [ "mcp__extm__echo" ];
            max_turns = 3;
            approve = false;
          };
          # A LAN MCP server: its host:port joins the allowlist of agents using it.
          sources.ext = {
            type = "mcp";
            url = "http://external:8080/mcp";
            read.resource = "x://a";
            allow_private = true;
          };
          agents.probe = {
            command = "egress-probe";
            prompt = "p";
            approve = false;
            mcp = [
              "ext"
              "stubsrc"
            ];
          };
          agents.ac = {
            kind = "claude";
            credential = "claude-max";
            prompt = "p";
            approve = false;
          };
          agents.ax = {
            kind = "codex";
            credential = "chatgpt";
            prompt = "p";
            approve = false;
          };
          agents.ag = {
            kind = "agy";
            credential = "google";
            prompt = "p";
            approve = false;
          };
          rules = [
            {
              name = "sandboxed-cmd";
              source = "gh";
              when = ''event.kind == "cmd"'';
              on = "each";
              id = "event.n";
              action.cmd = [
                "sh"
                "-c"
                "id -u; touch /var/lib/siphon/escape 2>/dev/null && echo ESCAPED || echo contained"
              ];
            }
            {
              name = "allowlisted-unit";
              source = "gh";
              when = ''event.kind == "unit"'';
              on = "each";
              id = "event.n";
              action.unit = "marker.service";
            }
            {
              name = "long-cmd";
              source = "gh";
              when = ''event.kind == "sleep"'';
              on = "each";
              id = "event.n";
              action.cmd = [
                "sleep"
                "600"
              ];
            }
            {
              name = "long-agent";
              source = "gh";
              when = ''event.kind == "agent"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "slow";
            }
            {
              name = "sub-claude";
              source = "gh";
              when = ''event.kind == "sub-claude"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "ac";
            }
            {
              name = "sub-codex";
              source = "gh";
              when = ''event.kind == "sub-codex"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "ax";
            }
            {
              name = "sub-agy";
              source = "gh";
              when = ''event.kind == "sub-agy"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "ag";
            }
            {
              name = "egress-agent";
              source = "gh";
              when = ''event.kind == "probe"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "probe";
            }
            {
              name = "gl-event";
              source = "glhook";
              when = ''event.object_kind == "merge_request"'';
              on = "each";
              id = "event.n";
              action.cmd = [
                "echo"
                "gitlab mr {{.event.n}}"
              ];
            }
            {
              name = "std-event";
              source = "stdhook";
              when = ''event.type == "ping"'';
              on = "each";
              id = "event.n";
              action.cmd = [
                "echo"
                "standard {{.event.n}}"
              ];
            }
            {
              name = "aws-agent";
              source = "gh";
              when = ''event.kind == "aws"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "awsagent";
            }
            {
              name = "bridge-agent";
              source = "gh";
              when = ''event.kind == "bridge"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "bridged";
            }
            {
              name = "model-agent";
              source = "gh";
              when = ''event.kind == "model"'';
              on = "each";
              id = "event.n";
              cooldown = "1s";
              action.agent = "local";
            }
            {
              name = "real-clis";
              source = "gh";
              when = ''event.kind == "real"'';
              on = "each";
              id = "event.n";
              action.cmd = [ "real-cli-check" ];
              egress.enabled = true;
            }
            {
              # cmd actions default to egress off: open template, no proxy.
              name = "open-cmd";
              source = "gh";
              when = ''event.kind == "open"'';
              on = "each";
              id = "event.n";
              action.cmd = [
                "curl"
                "-s"
                "-m"
                "10"
                "http://external:8080/"
              ];
            }
          ];
          units = [ "marker.service" ];
        };
      };

      systemd.services.marker.serviceConfig = {
        Type = "oneshot";
        StateDirectory = "marker";
        ExecStart = "${pkgs.coreutils}/bin/touch /var/lib/marker/done";
      };
      systemd.services.not-allowed.serviceConfig = {
        Type = "oneshot";
        ExecStart = "${pkgs.coreutils}/bin/true";
      };

      # Stand-in agent CLIs: each checks its login is where the runner puts
      # it, tries to read siphon's credential store (must fail), rewrites its
      # login with a marker (write-back), and answers.
      services.siphon.agentPackages = [
        # Runs in the restricted template: only the proxy is reachable.
        (pkgs.writeShellScriptBin "egress-probe" ''
          ip=$(awk '$2 == "external" || $3 == "external" {print $1; exit}' /etc/hosts)
          [ -e /run/nscd/socket ] && echo NSCD-VISIBLE
          grep -rqs '${stubToken}' "$HOME" /tmp /proc/*/environ 2>/dev/null && echo SECRET-LEAK
          [ -e /nix/var/nix/daemon-socket/socket ] && echo NIX-DAEMON-VISIBLE
          ls /run/systemd/units >/dev/null 2>&1 && echo UNIT-IDS-VISIBLE
          find /sys/fs/cgroup -name 'siphon-action*' 2>/dev/null | grep -q . && echo CGROUP-IDS-VISIBLE
          echo "slash-run=$(ls /run | tr '\n' ' ')"
          echo "via-proxy=$(curl -s -m 10 --proxytunnel http://external:8080/)"
          echo "blocked=$(curl -s -m 10 -o /dev/null -w '%{http_connect}' --proxytunnel http://blocked.example:8080/)"
          curl -s -m 5 --noproxy '*' "http://$ip:8080/" >/dev/null && echo RAW-IP-REACHED
          curl -s -m 5 --noproxy '*' http://127.0.0.1:8080/healthz >/dev/null && echo API-REACHED
          curl -s -m 5 --noproxy '*' http://127.77.0.1:8080/healthz >/dev/null && echo API-REACHED-77
          [ -e /run/dbus/system_bus_socket ] && echo DBUS-VISIBLE
          echo "proxy-env=''${HTTPS_PROXY:+set}"
        '')
        realClis
        (pkgs.writeShellScriptBin "slow-agent" "trap 'sleep 15; exit 0' TERM; sleep 600 & wait; wait")
        (pkgs.writeShellScriptBin "claude" ''
          f="$HOME/.claude/.credentials.json"
          [ -f "$f" ] || { echo "no login at $f" >&2; exit 2; }
          for p in /var/lib/siphon/credentials/claude-max/credentials.json /var/lib/siphon/credentials/chatgpt/auth.json /var/lib/siphon/credentials/google/antigravity-oauth-token; do cat "$p" >/dev/null 2>&1 && echo LEAK; done; [ -n "$(ls -A /etc/codex 2>/dev/null)" ] && echo ETC-VISIBLE
          cat >/dev/null
          printf '%s' '{"claudeAiOauth":{"accessToken":"a2","refreshToken":"refreshed-claude","expiresAt":4102444800000}}' > "$f"
          echo '{"type":"result","result":"claude ok"}'
        '')
        (pkgs.writeShellScriptBin "codex" ''
          f="$CODEX_HOME/auth.json"
          [ -f "$f" ] || { echo "no login at $f" >&2; exit 2; }
          for p in /var/lib/siphon/credentials/claude-max/credentials.json /var/lib/siphon/credentials/chatgpt/auth.json /var/lib/siphon/credentials/google/antigravity-oauth-token; do cat "$p" >/dev/null 2>&1 && echo LEAK; done; [ -n "$(ls -A /etc/codex 2>/dev/null)" ] && echo ETC-VISIBLE
          cat >/dev/null
          printf '%s' '{"tokens":{"id_token":"x","access_token":"h.eyJleHAiOjQxMDI0NDQ4MDB9.s","refresh_token":"refreshed-codex","account_id":"a"}}' > "$f"
          echo "codex ok"
        '')
        (pkgs.writeShellScriptBin "agy" ''
          f="$HOME/.gemini/antigravity-cli/antigravity-oauth-token"
          [ -f "$f" ] || { echo "no login at $f" >&2; exit 2; }
          for p in /var/lib/siphon/credentials/claude-max/credentials.json /var/lib/siphon/credentials/chatgpt/auth.json /var/lib/siphon/credentials/google/antigravity-oauth-token; do cat "$p" >/dev/null 2>&1 && echo LEAK; done; [ -n "$(ls -A /etc/codex 2>/dev/null)" ] && echo ETC-VISIBLE
          printf '%s' '{"token":{"access_token":"a2","token_type":"Bearer","refresh_token":"refreshed-agy","expiry":"2100-01-01T00:00:00Z"},"auth_method":"oauth"}' > "$f"
          echo '{"status":"OK","response":"agy ok"}'
        '')
      ];

      # A system-wide codex config the sandbox must not see.
      environment.etc."codex/config.toml".text = "";

      environment.systemPackages = [
        pkgs.sqlite
        pkgs.curl
        pkgs.openssl
        pkgs.jq
      ];
    };

  testScript = ''
    TOKEN = "test-token-0123456789abcdef0123456789"
    UNITS = "'siphon-action@*' 'siphon-action-open@*'"
    ACTIVE = f"systemctl list-units --no-legend --plain --state=active,activating,deactivating {UNITS}"

    def hook(body, sign=True):
        sig = ""
        if sign:
            mac = machine.succeed(
                f"printf '%s' '{body}' | openssl dgst -sha256 -hmac hook-secret | awk '{{print $2}}'"
            ).strip()
            sig = f"-H 'X-Hub-Signature-256: sha256={mac}'"
        return machine.succeed(
            f"curl -s -o /dev/null -w '%{{http_code}}' -X POST {sig} -d '{body}' http://127.0.0.1:8080/hook/gh"
        ).strip()

    def dump():
        print(machine.execute(f"curl -s -H 'Authorization: Bearer {TOKEN}' 'http://127.0.0.1:8080/api/jobs'")[1])
        print(machine.execute("sqlite3 /var/lib/siphon/state.db 'select id,rule,state,exit_code,output from jobs' 2>&1")[1])
        print(machine.execute("journalctl -u siphon -u 'siphon-action@*' -u 'siphon-action-open@*' -u marker -u polkit --no-pager | tail -80")[1])

    def denied_by_rule(cmd):
        # polkit's own rule must say no ("Access denied"), not merely the
        # non-interactive default ("Interactive authentication required").
        status, out = machine.execute(f"runuser -u siphon -- {cmd} 2>&1")
        assert status != 0, f"{cmd} succeeded"
        assert "Access denied" in out, f"{cmd}: not denied by the rule: {out}"

    machine.wait_for_unit("siphon.service")
    machine.wait_for_open_port(8080)

    with subtest("health and auth"):
        assert machine.succeed("curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz").strip() == "200"
        assert machine.succeed("curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/api/jobs").strip() == "401"

    with subtest("unsigned webhook is rejected"):
        assert hook('{"kind":"cmd","n":1}', sign=False) == "401", "unsigned webhook accepted"

    with subtest("signed webhooks run a sandboxed cmd and an allowlisted unit"):
        assert hook('{"kind":"cmd","n":1}') == "202"
        assert hook('{"kind":"unit","n":2}') == "202"
        try:
            machine.wait_until_succeeds("test -e /var/lib/marker/done", timeout=60)
            machine.wait_until_succeeds(
                "sqlite3 /var/lib/siphon/state.db \"select count(*) from jobs where state='done'\" | grep -qx 2",
                timeout=60,
            )
        except Exception:
            dump()
            raise
        out = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='sandboxed-cmd'\"")
        uid = out.split()[0]
        assert uid != "0" and "contained" in out, f"cmd not sandboxed: {out}"
        for leak in ("LEAK-DB", "LEAK-TOKEN", "ESCAPED"):
            assert leak not in out, f"sandbox leak {leak}: {out}"

    with subtest("polkit: only siphon-action@<hex> and the allowlist"):
        denied_by_rule("systemctl start not-allowed.service")
        denied_by_rule("systemctl restart marker.service")
        denied_by_rule("systemctl start siphon-action@not-hex.service")
        denied_by_rule("systemd-run --unit=evil.service true")
        # systemd itself refuses a transient unit shadowing the template (it has
        # a fragment file), before polkit is even asked; either refusal is fine.
        status, out = machine.execute(
            "runuser -u siphon -- systemd-run --wait -p User=root --unit=siphon-action@0123456789abcdef.service touch /root/pwned 2>&1"
        )
        assert status != 0 and ("fragment file" in out or "Access denied" in out), out
        machine.fail("test -e /root/pwned")
        denied_by_rule("systemctl enable not-allowed.service")
        denied_by_rule("systemctl daemon-reload")
        denied_by_rule("systemctl set-property siphon-action@0123456789abcdef.service CPUQuota=1%")

    with subtest("a symlink planted by siphon cannot redirect a root-opened file"):
        machine.succeed("echo original > /root/victim")
        run = "/var/lib/siphon-actions/fedcba9876543210"
        machine.succeed(f"install -d -m 2730 -o siphon -g siphon-io {run}")
        machine.succeed(f"echo '{{\"argv\":[\"echo\",\"pwned\"]}}' > {run}/job.json")
        machine.succeed(f"chown siphon:siphon-io {run}/job.json && chmod 640 {run}/job.json")
        machine.succeed(f"runuser -u siphon -- ln -s /root/victim {run}/stdout")
        machine.succeed(f"runuser -u siphon -- ln -s /root/victim2 {run}/stderr")
        machine.fail("runuser -u siphon -- systemctl start --wait siphon-action@fedcba9876543210.service")
        assert machine.succeed("cat /root/victim").strip() == "original", "symlink target was written"
        machine.fail("test -e /root/victim2")

    with subtest("subscription logins: claude, codex and agy run in the sandbox with write-back"):
        cfg = machine.succeed("systemctl cat siphon.service | grep -o '/nix/store/[a-z0-9]*-siphon.yaml' | head -1").strip()
        agw = machine.succeed("systemctl cat siphon.service | grep -o '/nix/store/[^ ]*/bin/siphon' | head -1").strip()
        logins = {
            "claude-max": '{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":4000000000000}}',
            "chatgpt": '{"tokens":{"id_token":"x","access_token":"h.eyJleHAiOjQwMDAwMDAwMDB9.s","refresh_token":"r1","account_id":"a"}}',
            "google": '{"token":{"access_token":"a1","token_type":"Bearer","refresh_token":"r1","expiry":"2096-01-01T00:00:00Z"},"auth_method":"oauth"}',
        }
        for name, body in logins.items():
            machine.succeed(f"printf '%s' '{body}' > /tmp/{name}.json")
            machine.succeed(f"runuser -u siphon -- {agw} credentials import -config {cfg} {name} < /tmp/{name}.json")
        for i, kind in enumerate(["sub-claude", "sub-codex", "sub-agy"]):
            assert hook(f'{{"kind":"{kind}","n":{100 + i}}}') == "202"
        try:
            for rule in ["sub-claude", "sub-codex", "sub-agy"]:
                machine.wait_until_succeeds(
                    f"sqlite3 /var/lib/siphon/state.db \"select state from jobs where rule='{rule}'\" | grep -qx done",
                    timeout=60,
                )
        except Exception:
            dump()
            raise
        outputs = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule like 'sub-%'\"")
        assert "LEAK" not in outputs, f"action read the credential store: {outputs}"
        assert "ETC-VISIBLE" not in outputs, f"system CLI config visible in the sandbox: {outputs}"
        for name, f, marker in [
            ("claude-max", "credentials.json", "refreshed-claude"),
            ("chatgpt", "auth.json", "refreshed-codex"),
            ("google", "antigravity-oauth-token", "refreshed-agy"),
        ]:
            machine.succeed(f"grep -q {marker} /var/lib/siphon/credentials/{name}/{f}")
        listing = machine.succeed(f"runuser -u siphon -- {agw} credentials ls -config {cfg}")
        assert "refreshed" not in listing and "r1" not in listing.split(), f"ls leaked a token: {listing}"

    with subtest("egress: restricted agents reach only allowlisted hosts via the proxy"):
        external.wait_for_open_port(8080)
        assert hook('{"kind":"probe","n":200}') == "202"
        assert hook('{"kind":"open","n":201}') == "202"
        try:
            for rule in ["egress-agent", "open-cmd"]:
                machine.wait_until_succeeds(
                    f"sqlite3 /var/lib/siphon/state.db \"select state from jobs where rule='{rule}'\" | grep -qx done",
                    timeout=60,
                )
        except Exception:
            dump()
            raise
        out = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='egress-agent'\"")
        assert "via-proxy=external-ok" in out, f"allowed host not reached via the proxy: {out}"
        assert "blocked=403" in out, f"disallowed host not refused: {out}"
        assert "egress: blocked blocked.example:8080 (1)" in out, f"blocked host not reported: {out}"
        assert "RAW-IP-REACHED" not in out, f"IP filter bypassed: {out}"
        assert "API-REACHED" not in out, f"siphon API reachable from the sandbox: {out}"
        assert "DBUS-VISIBLE" not in out, f"system bus reachable from the sandbox: {out}"
        assert "API-REACHED-77" not in out, f"wildcard-bound API reachable via the proxy address: {out}"
        assert "NSCD-VISIBLE" not in out, f"nscd (host name resolution) reachable from the sandbox: {out}"
        assert "SECRET-LEAK" not in out, f"a bridged MCP secret is visible to the agent: {out}"
        for leak in ("NIX-DAEMON-VISIBLE", "UNIT-IDS-VISIBLE", "CGROUP-IDS-VISIBLE"):
            assert leak not in out, f"{leak}: {out}"
        entries = set(out.split("slash-run=")[1].split("\n")[0].split())
        assert entries <= {"siphon", "current-system"}, f"unexpected /run entries in the sandbox: {entries}"
        assert "proxy-env=set" in out and "run-" not in out, f"proxy env missing or token leaked: {out}"
        n = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select count(*) from audit where event='egress_blocked' and detail='blocked.example:8080'\"").strip()
        assert n == "1", f"egress_blocked audited {n} times"
        out = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='open-cmd'\"")
        assert "external-ok" in out, f"cmd without egress could not reach the network: {out}"

    with subtest("real claude and codex start in the restricted sandbox and reach only the proxy"):
        assert hook('{"kind":"real","n":300}') == "202"
        try:
            machine.wait_until_succeeds(
                "sqlite3 /var/lib/siphon/state.db \"select state from jobs where rule='real-clis'\" | grep -qx done",
                timeout=300,
            )
        except Exception:
            dump()
            raise
        out = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='real-clis'\"")
        print(out)
        assert "claude-version-rc=0" in out and "codex-version-rc=0" in out, f"a CLI failed to start: {out}"
        for crash in ("uv_os_get_passwd", "getpwuid", "No user exists"):
            assert crash not in out, f"user lookup failed without nscd: {out}"
        assert "egress: blocked api.anthropic.com:443" in out, f"claude never reached the proxy: {out}"
        assert "egress: blocked api.openai.com:443" in out, f"codex never reached the proxy: {out}"

    with subtest("a model agent (built-in loop) calls an MCP tool through the sandbox"):
        external.wait_for_open_port(8000)
        assert hook('{"kind":"model","n":600}') == "202"
        try:
            machine.wait_until_succeeds(
                "sqlite3 /var/lib/siphon/state.db \"select state from jobs where rule='model-agent'\" | grep -qx done",
                timeout=120,
            )
        except Exception:
            dump()
            print(machine.execute("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='model-agent'\"")[1])
            raise
        out = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='model-agent'\"")
        assert "final: echo: hi from the model" in out, out
        external.succeed("grep -q 'hi from the model' /tmp/mcp-calls")

    with subtest("webhook auth: GitLab token header and Standard Webhooks"):
        import base64, hashlib, hmac, json, time
        def post(path, body, headers):
            h = " ".join(f"-H '{k}: {v}'" for k, v in headers.items())
            return machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' -X POST {h} -d '{body}' http://127.0.0.1:8080/hook/{path}").strip()
        gl = '{"object_kind":"merge_request","n":801}'
        assert post("glhook", gl, {"X-Gitlab-Token": "wrong"}) == "401"
        assert post("glhook", gl, {}) == "401"
        assert post("glhook", gl, {"X-Gitlab-Token": "gl-hook-token-123"}) == "202"
        key = base64.b64decode("c2lwaG9uLXN0YW5kYXJkLXdlYmhvb2tzLWtleQ==")
        body, mid, ts = '{"type":"ping","n":802}', "msg_vm_1", str(int(time.time()))
        sig = "v1," + base64.b64encode(hmac.new(key, f"{mid}.{ts}.{body}".encode(), hashlib.sha256).digest()).decode()
        hdr = {"webhook-id": mid, "webhook-timestamp": ts, "webhook-signature": sig}
        assert post("stdhook", body, {**hdr, "webhook-signature": "v1,AAAA"}) == "401"
        assert post("stdhook", body, {**hdr, "webhook-timestamp": str(int(time.time()) - 900)}) == "401"
        assert post("stdhook", body, hdr) == "202"
        assert post("stdhook", body, hdr) != "202", "replayed delivery accepted"
        for rule, text in [("gl-event", "gitlab mr 801"), ("std-event", "standard 802")]:
            machine.wait_until_succeeds(
                f"sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='{rule}' and state='done'\" | grep -q '{text}'",
                timeout=60,
            )
        logs = machine.succeed("journalctl -u siphon --no-pager")
        assert "gl-hook-token-123" not in logs, "webhook token in the logs"

    with subtest("MCP bridge: a stdio server's secret reaches the server, never the agent"):
        assert hook('{"kind":"bridge","n":700}') == "202"
        try:
            machine.wait_until_succeeds(
                "sqlite3 /var/lib/siphon/state.db \"select state from jobs where rule='bridge-agent'\" | grep -qx done",
                timeout=120,
            )
        except Exception:
            dump()
            print(machine.execute("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='bridge-agent'\"")[1])
            raise
        out = machine.succeed("sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='bridge-agent'\"")
        import hashlib
        want = hashlib.sha256(b"${stubToken}").hexdigest()[:16]
        assert f"token-sha={want}" in out, f"the bridged server didn't get its secret: {out}"
        assert "${stubToken}" not in out, f"the secret leaked into the job output: {out}"
        machine.fail("systemctl list-units --no-legend --plain 'siphon-mcp@*' | grep -q running")

    with subtest("AWS: a run's bridge gets short-lived keys from STS; the base key goes nowhere"):
        import hashlib
        assert hook('{"kind":"aws","n":710}') == "202"
        q = "sqlite3 /var/lib/siphon/state.db \"select {} from jobs where rule='aws-agent'\""
        # While the run is live (the stub holds its tool call for 10 s): the agent's
        # unit, run dir and process environment hold no AWS key, temporary or base.
        machine.wait_until_succeeds("systemctl list-units --no-legend --plain --state=running 'siphon-mcp@*' | grep -q .", timeout=60)
        machine.wait_until_succeeds("systemctl list-units --no-legend --plain --state=running 'siphon-action@*' | grep -q .", timeout=60)
        live = "temp-secret-vm-test|temp-token-vm-test|ASIATEMPVMTEST|base-secret-NEVER-vm|AKIABASEVMTEST"
        machine.fail(f"grep -rE '{live}' /var/lib/siphon-actions")
        envs = machine.succeed(
            "for u in $(systemctl list-units --no-legend --plain --state=running 'siphon-action@*' | cut -d' ' -f1); do"
            " p=$(systemctl show -p MainPID --value $u); tr '\\0' '\\n' </proc/$p/environ; systemctl show -p Environment $u; done"
        )
        assert "PATH=" in envs, f"could not read the agent's environment: {envs}"
        for leak in live.split("|"):
            assert leak not in envs, f"{leak} is in the agent unit's environment"
        try:
            machine.wait_until_succeeds(q.format("state") + " | grep -qx done", timeout=120)
        except Exception:
            dump()
            print(machine.execute(q.format("output"))[1])
            raise
        out = machine.succeed(q.format("output"))
        h = lambda v: hashlib.sha256(v.encode()).hexdigest()[:16]
        for want in (f"aws-akid-sha={h('ASIATEMPVMTEST')}", f"aws-secret-sha={h('temp-secret-vm-test')}",
                     f"aws-token-sha={h('temp-token-vm-test')}", "aws-region=eu-west-1", "aws-imds-off=true",
                     "aws-config=/dev/null", "aws-creds-file=/dev/null"):
            assert want in out, f"bridge env: missing {want}: {out}"
        sts = machine.succeed("cat /tmp/sts-requests")
        jid = machine.succeed(q.format("id")).strip()
        for want in ("Action=AssumeRole", f"RoleSessionName=siphon-{jid}", "DurationSeconds=900", "ExternalId=vm-ext"):
            assert want in sts, f"STS request: missing {want}: {sts}"
        # The base secret never leaves the daemon; the temporary ones are masked.
        for leak in ("base-secret-NEVER-vm", "temp-secret-vm-test", "temp-token-vm-test"):
            assert leak not in out, f"{leak} in the job output"
        machine.fail("grep -r base-secret-NEVER-vm /var/lib/siphon/jobs 2>/dev/null")
        bridge_logs = machine.succeed("journalctl -u 'siphon-mcp@*' --no-pager")
        for leak in ("base-secret-NEVER-vm", "temp-secret-vm-test", "temp-token-vm-test"):
            assert leak not in bridge_logs, f"{leak} in the bridge's journal"
        logs = machine.succeed("journalctl -u siphon --no-pager")
        assert "base-secret-NEVER-vm" not in logs and "temp-secret-vm-test" not in logs, "an AWS secret is in the logs"
        machine.succeed("test -z \"$(ls -A /var/lib/siphon/bridge-secrets 2>/dev/null)\"")

    with subtest("CLI walkthrough as a normal user (docs/getting-started.md)"):
        import json, shlex, time
        def alice(cmd):
            # stdin from /dev/null: a stray prompt fails fast (exit 2 + hint) instead of hanging
            return machine.succeed("su - alice -c " + shlex.quote(cmd) + " < /dev/null")
        def wait_job(rule, state, timeout=120):
            end = time.time() + timeout
            while time.time() < end:
                jobs = json.loads(alice("siphon jobs -o json"))
                hit = [j for j in jobs if j["rule"] == rule and j["state"] == state]
                if hit:
                    return hit[0]
                time.sleep(1)
            raise Exception(f"{rule} never reached {state}: {jobs}")
        # cli.enable: on PATH, SIPHON_URL points at the daemon
        alice('command -v siphon && test "$SIPHON_URL" = http://127.0.0.1:8080')
        alice('siphon login "$SIPHON_URL" < /etc/siphon/token')
        assert alice("stat -c %a ~/.config/siphon/client.yaml").strip() == "600"
        out = alice("siphon connect model openai --name vm-model --url http://external:8000/v1")
        assert "test: ok" in out and "stub-model" in out, out
        # first task: webhook -> command
        t = json.loads(alice("""siphon new task --name vmhello --webhook vm-hook --when 'event.msg != nil' --on each --id event.msg --cmd '["echo","vm {{.event.msg}}"]' --yes -o json"""))
        sec = t["webhook"]["secret"]
        sec_hello = sec  # later subtests reuse the name sec
        machine.succeed(f"""curl -sf -H 'X-Siphon-Key: {sec}' -d '{{"msg":"hi"}}' http://127.0.0.1:8080/hook/vm-hook""")
        wait_job("vmhello", "done")
        last = json.loads(alice("siphon test vmhello --last -o json"))
        assert last["fires"][0]["argv"] == ["echo", "vm hi"], last
        why = json.loads(alice("siphon why vmhello -o json"))
        assert "fired" in [r["code"] for r in why["reasons"]], why
        # a template validates against the live config
        dry = json.loads(alice("siphon template disk-full > ~/disk.yaml && siphon apply -f ~/disk.yaml --dry-run -o json"))
        assert dry["dry_run"] and not dry["errors"], dry
        # an agent task: approved from the CLI, runs on the model connection
        alice("""printf '%s\\n' 'agents:' '  vm-agent: { kind: model, credential: vm-model, model: stub-model, prompt: "say hi", max_turns: 2, timeout: 2m }' 'rules:' '  - { name: vm-agent-rule, source: vm-hook, when: "event.agent == true", on: each, id: event.n, cooldown: 1s, action: { agent: vm-agent } }' > ~/agent.yaml""")
        alice("siphon apply -f ~/agent.yaml --yes")
        machine.succeed(f"""curl -sf -H 'X-Siphon-Key: {sec}' -d '{{"agent":true,"n":1}}' http://127.0.0.1:8080/hook/vm-hook""")
        job = wait_job("vm-agent-rule", "pending_approval")
        alice(f"siphon approve {job['id']}")
        wait_job("vm-agent-rule", "done")
        hist = alice("siphon history -o json")
        assert "api:cli:alice" in hist, hist
        # siphon mcp: tools listed, none can approve
        tools = alice("${pkgs.python3}/bin/python3 ${./mcp-probe.py} siphon mcp").split()
        assert "why" in tools and "apply" in tools and "draft" in tools, tools
        assert not any("approve" in t or "deny" in t for t in tools), tools
        # the portal's Help & Docs and llms.txt
        machine.succeed(f"curl -s -c /tmp/help.jar -o /dev/null --data-urlencode token={TOKEN} http://127.0.0.1:8080/login")
        assert "Get started" in machine.succeed("curl -sf -b /tmp/help.jar http://127.0.0.1:8080/help")
        assert "# Siphon" in machine.succeed("curl -sf -b /tmp/help.jar http://127.0.0.1:8080/llms.txt")

    with subtest("a schedule source fires on its own (docs/tasks/schedule.md)"):
        import json
        alice("""siphon new task --name tick --schedule 'every 1m' --cmd '["echo","tick"]' --yes""")
        wait_job("tick", "done", timeout=150)
        line = [l for l in alice("siphon get sources").splitlines() if l.startswith("tick ")]
        assert line and "20" in line[0], f"no NEXT run for tick: {line}"   # a dated next run
        why = json.loads(alice("siphon why tick -o json"))
        assert why["source"]["schedule"]["next_run_at"], why
        # stop ticking: a stray minute job would break the orphan-count subtests below
        alice("siphon delete rules tick")
        alice("siphon delete sources tick")

    with subtest("catalogue services: connect from the CLI, verified deliveries (docs/connections)"):
        import json, hashlib, hmac, time as _t
        # Uptime Kuma: a token webhook; the generated secret is shown once
        up = json.loads(alice("siphon connect uptime-kuma --name kuma --no-test -o json"))["connected"]
        assert up["hook_secret"] and up["hook_url"].endswith("/hook/kuma"), up
        alice("""siphon new task --name kuma-down --source kuma --when 'event.heartbeat.status == 0' --on each --id 'event.monitor.name' --cmd '["echo","down {{.event.monitor.name}}"]' --yes""")
        machine.succeed(f"""curl -sf -H 'X-Siphon-Key: {up["hook_secret"]}' -d '{{"heartbeat":{{"status":0}},"monitor":{{"name":"web"}}}}' http://127.0.0.1:8080/hook/kuma""")
        wait_job("kuma-down", "done")
        # Slack: the signing secret on stdin; url_verification answered; a signed event fires
        sec = "slack-signing-secret-vm"
        machine.succeed(f"printf '%s' {sec} > /tmp/slack.sec && chmod 644 /tmp/slack.sec")
        alice("siphon connect slack --name slack --signing-secret @/tmp/slack.sec --no-test")
        def slack_post(body):
            ts = str(int(_t.time()))
            sig = "v0=" + hmac.new(sec.encode(), f"v0:{ts}:{body}".encode(), hashlib.sha256).hexdigest()
            machine.succeed(f"printf '%s' '{body}' > /tmp/slack.body")
            return machine.succeed(f"curl -s -w ' %{{http_code}}' -H 'X-Slack-Request-Timestamp: {ts}' -H 'X-Slack-Signature: {sig}' --data-binary @/tmp/slack.body http://127.0.0.1:8080/hook/slack").strip()
        assert slack_post('{"type":"url_verification","challenge":"vm-chal"}') == "vm-chal 200"
        alice("""siphon new task --name slack-mention --source slack --when 'event.event.type == "app_mention"' --on each --id event.event_id --cmd '["echo","mention"]' --yes""")
        assert slack_post('{"type":"event_callback","event_id":"Ev1","event":{"type":"app_mention","text":"hi"}}').endswith("202")
        wait_job("slack-mention", "done")
        # the portal groups both connections
        machine.succeed(f"curl -s -c /tmp/svc.jar -o /dev/null --data-urlencode token={TOKEN} http://127.0.0.1:8080/login")
        page = machine.succeed("curl -sf -b /tmp/svc.jar http://127.0.0.1:8080/services")
        assert "Uptime Kuma" in page and "Slack" in page and "Explore services" in page

    with subtest("notifications: approval and failure reach a webhook channel, once (docs/tasks/notifications.md)"):
        import json, time as _t
        def notes():
            out = machine.succeed("cat /tmp/notify.log 2>/dev/null || true")
            return [json.loads(l) for l in out.splitlines() if l.strip()]
        def wait_note(pred, timeout=60):
            end = _t.time() + timeout
            while _t.time() < end:
                hit = [n for n in notes() if pred(n)]
                if hit:
                    return hit
                _t.sleep(2)
            raise Exception(f"no such notification: {notes()}")
        machine.wait_for_open_port(18099)
        alice("printf %s http://127.0.0.1:18099/n | siphon notify add hook --type webhook --url - --events approval,failed --yes")
        assert "sent" in alice("siphon notify test hook")
        assert [n["event"] for n in notes()] == ["test"], notes()
        # an agent run waits for approval -> exactly one message for it
        machine.succeed(f"""curl -sf -H 'X-Siphon-Key: {sec_hello}' -d '{{"agent":true,"n":2}}' http://127.0.0.1:8080/hook/vm-hook""")
        job = wait_job("vm-agent-rule", "pending_approval")
        appr = wait_note(lambda n: n["event"] == "approval")
        assert len(appr) == 1 and appr[0]["job"] == job["id"] and appr[0]["rule"] == "vm-agent-rule", appr
        alice(f"siphon deny {job['id']}")
        # a failing command -> one "failed" message
        alice("""siphon new task --name vm-fails --source vm-hook --when 'event.fail == true' --on each --id event.n --cmd '["false"]' --yes""")
        machine.succeed(f"""curl -sf -H 'X-Siphon-Key: {sec_hello}' -d '{{"fail":true,"n":1}}' http://127.0.0.1:8080/hook/vm-hook""")
        fjob = wait_job("vm-fails", "failed")
        assert len(wait_note(lambda n: n["event"] == "failed" and n["job"] == fjob["id"])) == 1
        # a restart re-sends nothing: the outbox remembers what was delivered
        before = len(notes())
        machine.succeed("systemctl restart siphon.service")
        machine.wait_for_open_port(8080)
        _t.sleep(35)  # two scans
        assert len(notes()) == before, notes()[before:]
        log = json.loads(alice("siphon notify log -o json"))
        rows = log if isinstance(log, list) else log.get("notifications", [])
        mine = [r for r in rows if r["channel"] == "hook"]
        assert mine and all(r["state"] == "sent" for r in mine), rows
        # leave nothing behind for the later subtests
        alice("siphon delete rules vm-fails")
        alice("siphon delete notify hook")

    with subtest("backup, offline restore and metrics (docs/tasks/backup-and-monitoring.md)"):
        alice("""siphon new task --name vm-kept --source vm-hook --when 'event.keep == true' --on each --id event.n --cmd '["true"]' --yes""")
        alice("printf %s http://127.0.0.1:18099/k | siphon notify add kept --type webhook --url - --events failed --yes")
        # the timer's unit, run now: one 0600 archive with the db and the pasted secret
        machine.succeed("systemctl start siphon-backup.service")
        archives = machine.succeed("ls /var/backup/siphon/siphon-*.tar.gz").split()
        assert len(archives) == 1, archives
        assert machine.succeed(f"stat -c %a {archives[0]}").strip() == "600"
        listing = machine.succeed(f"tar -tzf {archives[0]}")
        for want in ("siphon-backup.json", "state.db", "secrets/notify--kept+url"):
            assert want in listing, listing
        # restore refuses while siphon runs (exit 5), then works offline
        status, out = machine.execute(f"runuser -u siphon -- siphon backup restore -db /var/lib/siphon/state.db --yes {archives[0]} 2>&1")
        assert status == 5, (status, out)
        machine.succeed("systemctl stop siphon.service")
        machine.succeed("rm -rf /var/lib/siphon/state.db /var/lib/siphon/state.db-wal /var/lib/siphon/state.db-shm /var/lib/siphon/secrets")
        machine.succeed(f"runuser -u siphon -- siphon backup restore -db /var/lib/siphon/state.db --yes {archives[0]}")
        machine.succeed("systemctl start siphon.service")
        machine.wait_for_open_port(8080)
        alice("siphon get rules vm-kept")  # exit 4 if the restore lost it
        machine.succeed("test -f /var/lib/siphon/secrets/notify--kept+url")
        # /metrics: the login token, like the API
        metrics = "http://127.0.0.1:8080/metrics"
        assert machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' {metrics}").strip() == "401"
        body = machine.succeed(f"curl -sf -H 'Authorization: Bearer {TOKEN}' {metrics}")
        assert 'siphon_jobs{state="done"}' in body and "# TYPE siphon_approvals_pending gauge" in body, body
        # The scrape token opens /metrics and nothing else.
        scrape = "-H 'Authorization: Bearer scrape-token-0123456789abcdef0123456789'"
        machine.succeed(f"curl -sf {scrape} {metrics} >/dev/null")
        assert machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' {scrape} http://127.0.0.1:8080/api/inventory").strip() == "401"
        alice("siphon delete rules vm-kept")
        alice("siphon delete notify kept")
        machine.succeed("rm -rf /var/lib/siphon/pre-restore-* /var/backup/siphon/siphon-*.tar.gz")

    with subtest("a rule created over the config API fires without a restart"):
        import json
        auth = f"-H 'Authorization: Bearer {TOKEN}'"
        api = "http://127.0.0.1:8080/api/config/rules"
        rev = json.loads(machine.succeed(f"curl -sf {auth} {api}/sandboxed-cmd"))["rev"]
        # Earlier subtests (the CLI walkthrough) also change the config: count this one's effect.
        count = lambda q: int(machine.succeed(f"sqlite3 /var/lib/siphon/state.db \"{q}\"").strip())
        revs0 = count("select count(*) from config_revision")
        changed0 = count("select count(*) from audit where event='config_changed'")
        def put(name, yaml_text, rev):
            body = json.dumps({"yaml": yaml_text, "rev": rev})
            machine.succeed(f"printf '%s' '{body}' > /tmp/put.json")
            return machine.succeed(f"curl -s -o /tmp/put.out -w '%{{http_code}}' -X PUT {auth} -H 'Content-Type: application/json' --data-binary @/tmp/put.json {api}/{name}").strip()
        # Invalid (unknown source): refused, nothing stored.
        assert put("bad-rule", "source: nope\nwhen: 'true'\naction: {cmd: [echo, x]}\n", rev) in ("400", "422"), machine.succeed("cat /tmp/put.out")
        code = put("portal-rule", "source: gh\nwhen: event.kind == \"portal\"\non: each\nid: event.n\naction: {cmd: [echo, from-portal]}\n", rev)
        assert code == "200", machine.succeed("cat /tmp/put.out")
        assert hook('{"kind":"portal","n":500}') == "202"
        try:
            machine.wait_until_succeeds(
                "sqlite3 /var/lib/siphon/state.db \"select output from jobs where rule='portal-rule' and state='done'\" | grep -q from-portal",
                timeout=60,
            )
        except Exception:
            dump()
            raise
        assert count("select count(*) from config_revision") == revs0 + 1
        assert count("select count(*) from audit where event='config_changed'") == changed0 + 1

    with subtest("an OAuth MCP source: status, a failing login start and the public callback (docs/tasks/connect-oauth-mcp.md)"):
        import json
        auth = f"-H 'Authorization: Bearer {TOKEN}'"
        base = "http://127.0.0.1:8080"
        st = json.loads(machine.succeed(f"curl -sf {auth} {base}/api/sources/oauthsrc/oauth"))
        assert st["status"] == "none", st
        # Nothing listens on :9: starting a login fails fast instead of hanging.
        code = machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' -m 40 -X POST {auth} {base}/api/sources/oauthsrc/oauth/login").strip()
        assert code == "502", code
        # The callback needs no token, and refuses a state it never issued.
        code = machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' '{base}/oauth/callback?state=bogus&code=x'").strip()
        assert code == "400", code
        machine.succeed("test ! -e /var/lib/siphon/credentials/.mcp/oauthsrc")

    with subtest("portal SSO: the button, an unreachable provider and a bogus callback (docs/tasks/sso.md)"):
        base = "http://127.0.0.1:8080"
        page = machine.succeed(f"curl -sf {base}/login")
        assert "Sign in with SSO" in page and 'name="token"' in page, page
        # Discovery fails fast instead of hanging, and the error is shown.
        code = machine.succeed(f"curl -s -o /tmp/sso.html -w '%{{http_code}}' -m 40 {base}/login/oidc").strip()
        assert code == "502", code
        machine.succeed("grep -q 'SSO provider unreachable' /tmp/sso.html")
        # No sign-in in progress: refused (one failure on the shared limiter budget).
        code = machine.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' '{base}/login/oidc/callback?state=x&code=y'").strip()
        assert code == "400", code
        # The token login is the break-glass path and still works.
        machine.succeed(f"curl -s -c /tmp/sso.jar -o /dev/null --data-urlencode token={TOKEN} {base}/login")
        machine.succeed("grep -q siphon_session /tmp/sso.jar")

    with subtest("stopping siphon leaves no orphaned action units (cmd and agent)"):
        assert hook('{"kind":"sleep","n":3}') == "202"
        assert hook('{"kind":"agent","n":4}') == "202"
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=60)
        machine.succeed("systemctl stop siphon.service")
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 0", timeout=30)

    with subtest("after a crash, orphans are stopped before their jobs are requeued"):
        machine.succeed("systemctl start siphon.service")
        machine.wait_for_open_port(8080)
        # The startup requeue reruns the two interrupted jobs.
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=90)
        before = set(machine.succeed(f"{ACTIVE} | awk '{{print $1}}'").split())
        machine.succeed("systemctl kill -s KILL siphon.service")
        # Sample through the restart: the agent takes 15 s to stop, so a requeue
        # that didn't wait would show 3-4 instances at once.
        peak = 0
        for _ in range(40):
            peak = max(peak, int(machine.succeed(f"{ACTIVE} | wc -l").strip()))
            machine.sleep(1)
        assert peak <= 2, f"{peak} instances at once: a step ran twice concurrently"
        machine.wait_until_succeeds("systemctl is-active siphon.service", timeout=60)
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=90)
        after = set(machine.succeed(f"{ACTIVE} | awk '{{print $1}}'").split())
        assert not (before & after), f"orphans still running: {before & after}"
        failed = machine.succeed(f"systemctl list-units --failed --no-legend --plain {UNITS} | wc -l").strip()
        assert failed == "0", f"{failed} failed action units left loaded"
  '';
}
