{ self, pkgs }:

let
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
  name = "agentgw";

  # A host outside the sandbox: the only way to it from a restricted action
  # is agentgw's egress proxy.
  nodes.external = {
    networking.firewall.allowedTCPPorts = [ 8080 ];
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
      environment.etc."agentgw/token".text = "test-token-0123456789abcdef0123456789";
      environment.etc."agentgw/hook".text = "hook-secret";

      services.agentgw = {
        enable = true;
        credentials = {
          token = "/etc/agentgw/token";
          hook = "/etc/agentgw/hook";
        };
        settings = {
          server = {
            # All addresses: the restricted sandbox must still not reach it.
            listen = "0.0.0.0:8080";
            token = "file:/run/credentials/agentgw.service/token";
          };
          sources.gh = {
            type = "webhook";
            secret = "file:/run/credentials/agentgw.service/hook";
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
              api_key = "file:/etc/agentgw/token";
            };
            claude-max.provider = "claude";
            chatgpt.provider = "codex";
            google.provider = "agy";
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
            mcp = [ "ext" ];
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
                "id -u; touch /var/lib/agentgw/escape 2>/dev/null && echo ESCAPED || echo contained"
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
      # it, tries to read agentgw's credential store (must fail), rewrites its
      # login with a marker (write-back), and answers.
      services.agentgw.agentPackages = [
        # Runs in the restricted template: only the proxy is reachable.
        (pkgs.writeShellScriptBin "egress-probe" ''
          ip=$(awk '$2 == "external" || $3 == "external" {print $1; exit}' /etc/hosts)
          [ -e /run/nscd/socket ] && echo NSCD-VISIBLE
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
          for p in /var/lib/agentgw/credentials/claude-max/credentials.json /var/lib/agentgw/credentials/chatgpt/auth.json /var/lib/agentgw/credentials/google/antigravity-oauth-token; do cat "$p" >/dev/null 2>&1 && echo LEAK; done; [ -n "$(ls -A /etc/codex 2>/dev/null)" ] && echo ETC-VISIBLE
          cat >/dev/null
          printf '%s' '{"claudeAiOauth":{"accessToken":"a2","refreshToken":"refreshed-claude","expiresAt":4102444800000}}' > "$f"
          echo '{"type":"result","result":"claude ok"}'
        '')
        (pkgs.writeShellScriptBin "codex" ''
          f="$CODEX_HOME/auth.json"
          [ -f "$f" ] || { echo "no login at $f" >&2; exit 2; }
          for p in /var/lib/agentgw/credentials/claude-max/credentials.json /var/lib/agentgw/credentials/chatgpt/auth.json /var/lib/agentgw/credentials/google/antigravity-oauth-token; do cat "$p" >/dev/null 2>&1 && echo LEAK; done; [ -n "$(ls -A /etc/codex 2>/dev/null)" ] && echo ETC-VISIBLE
          cat >/dev/null
          printf '%s' '{"tokens":{"id_token":"x","access_token":"h.eyJleHAiOjQxMDI0NDQ4MDB9.s","refresh_token":"refreshed-codex","account_id":"a"}}' > "$f"
          echo "codex ok"
        '')
        (pkgs.writeShellScriptBin "agy" ''
          f="$HOME/.gemini/antigravity-cli/antigravity-oauth-token"
          [ -f "$f" ] || { echo "no login at $f" >&2; exit 2; }
          for p in /var/lib/agentgw/credentials/claude-max/credentials.json /var/lib/agentgw/credentials/chatgpt/auth.json /var/lib/agentgw/credentials/google/antigravity-oauth-token; do cat "$p" >/dev/null 2>&1 && echo LEAK; done; [ -n "$(ls -A /etc/codex 2>/dev/null)" ] && echo ETC-VISIBLE
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
    UNITS = "'agentgw-action@*' 'agentgw-action-open@*'"
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
        print(machine.execute("sqlite3 /var/lib/agentgw/state.db 'select id,rule,state,exit_code,output from jobs' 2>&1")[1])
        print(machine.execute("journalctl -u agentgw -u 'agentgw-action@*' -u 'agentgw-action-open@*' -u marker -u polkit --no-pager | tail -80")[1])

    def denied_by_rule(cmd):
        # polkit's own rule must say no ("Access denied"), not merely the
        # non-interactive default ("Interactive authentication required").
        status, out = machine.execute(f"runuser -u agentgw -- {cmd} 2>&1")
        assert status != 0, f"{cmd} succeeded"
        assert "Access denied" in out, f"{cmd}: not denied by the rule: {out}"

    machine.wait_for_unit("agentgw.service")
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
                "sqlite3 /var/lib/agentgw/state.db \"select count(*) from jobs where state='done'\" | grep -qx 2",
                timeout=60,
            )
        except Exception:
            dump()
            raise
        out = machine.succeed("sqlite3 /var/lib/agentgw/state.db \"select output from jobs where rule='sandboxed-cmd'\"")
        uid = out.split()[0]
        assert uid != "0" and "contained" in out, f"cmd not sandboxed: {out}"
        for leak in ("LEAK-DB", "LEAK-TOKEN", "ESCAPED"):
            assert leak not in out, f"sandbox leak {leak}: {out}"

    with subtest("polkit: only agentgw-action@<hex> and the allowlist"):
        denied_by_rule("systemctl start not-allowed.service")
        denied_by_rule("systemctl restart marker.service")
        denied_by_rule("systemctl start agentgw-action@not-hex.service")
        denied_by_rule("systemd-run --unit=evil.service true")
        # systemd itself refuses a transient unit shadowing the template (it has
        # a fragment file), before polkit is even asked; either refusal is fine.
        status, out = machine.execute(
            "runuser -u agentgw -- systemd-run --wait -p User=root --unit=agentgw-action@0123456789abcdef.service touch /root/pwned 2>&1"
        )
        assert status != 0 and ("fragment file" in out or "Access denied" in out), out
        machine.fail("test -e /root/pwned")
        denied_by_rule("systemctl enable not-allowed.service")
        denied_by_rule("systemctl daemon-reload")
        denied_by_rule("systemctl set-property agentgw-action@0123456789abcdef.service CPUQuota=1%")

    with subtest("a symlink planted by agentgw cannot redirect a root-opened file"):
        machine.succeed("echo original > /root/victim")
        run = "/var/lib/agentgw-actions/fedcba9876543210"
        machine.succeed(f"install -d -m 2730 -o agentgw -g agentgw-io {run}")
        machine.succeed(f"echo '{{\"argv\":[\"echo\",\"pwned\"]}}' > {run}/job.json")
        machine.succeed(f"chown agentgw:agentgw-io {run}/job.json && chmod 640 {run}/job.json")
        machine.succeed(f"runuser -u agentgw -- ln -s /root/victim {run}/stdout")
        machine.succeed(f"runuser -u agentgw -- ln -s /root/victim2 {run}/stderr")
        machine.fail("runuser -u agentgw -- systemctl start --wait agentgw-action@fedcba9876543210.service")
        assert machine.succeed("cat /root/victim").strip() == "original", "symlink target was written"
        machine.fail("test -e /root/victim2")

    with subtest("subscription logins: claude, codex and agy run in the sandbox with write-back"):
        cfg = machine.succeed("systemctl cat agentgw.service | grep -o '/nix/store/[a-z0-9]*-agentgw.yaml' | head -1").strip()
        agw = machine.succeed("systemctl cat agentgw.service | grep -o '/nix/store/[^ ]*/bin/agentgw' | head -1").strip()
        logins = {
            "claude-max": '{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":4000000000000}}',
            "chatgpt": '{"tokens":{"id_token":"x","access_token":"h.eyJleHAiOjQwMDAwMDAwMDB9.s","refresh_token":"r1","account_id":"a"}}',
            "google": '{"token":{"access_token":"a1","token_type":"Bearer","refresh_token":"r1","expiry":"2096-01-01T00:00:00Z"},"auth_method":"oauth"}',
        }
        for name, body in logins.items():
            machine.succeed(f"printf '%s' '{body}' > /tmp/{name}.json")
            machine.succeed(f"runuser -u agentgw -- {agw} credentials import -config {cfg} {name} < /tmp/{name}.json")
        for i, kind in enumerate(["sub-claude", "sub-codex", "sub-agy"]):
            assert hook(f'{{"kind":"{kind}","n":{100 + i}}}') == "202"
        try:
            for rule in ["sub-claude", "sub-codex", "sub-agy"]:
                machine.wait_until_succeeds(
                    f"sqlite3 /var/lib/agentgw/state.db \"select state from jobs where rule='{rule}'\" | grep -qx done",
                    timeout=60,
                )
        except Exception:
            dump()
            raise
        outputs = machine.succeed("sqlite3 /var/lib/agentgw/state.db \"select output from jobs where rule like 'sub-%'\"")
        assert "LEAK" not in outputs, f"action read the credential store: {outputs}"
        assert "ETC-VISIBLE" not in outputs, f"system CLI config visible in the sandbox: {outputs}"
        for name, f, marker in [
            ("claude-max", "credentials.json", "refreshed-claude"),
            ("chatgpt", "auth.json", "refreshed-codex"),
            ("google", "antigravity-oauth-token", "refreshed-agy"),
        ]:
            machine.succeed(f"grep -q {marker} /var/lib/agentgw/credentials/{name}/{f}")
        listing = machine.succeed(f"runuser -u agentgw -- {agw} credentials ls -config {cfg}")
        assert "refreshed" not in listing and "r1" not in listing.split(), f"ls leaked a token: {listing}"

    with subtest("egress: restricted agents reach only allowlisted hosts via the proxy"):
        external.wait_for_open_port(8080)
        assert hook('{"kind":"probe","n":200}') == "202"
        assert hook('{"kind":"open","n":201}') == "202"
        try:
            for rule in ["egress-agent", "open-cmd"]:
                machine.wait_until_succeeds(
                    f"sqlite3 /var/lib/agentgw/state.db \"select state from jobs where rule='{rule}'\" | grep -qx done",
                    timeout=60,
                )
        except Exception:
            dump()
            raise
        out = machine.succeed("sqlite3 /var/lib/agentgw/state.db \"select output from jobs where rule='egress-agent'\"")
        assert "via-proxy=external-ok" in out, f"allowed host not reached via the proxy: {out}"
        assert "blocked=403" in out, f"disallowed host not refused: {out}"
        assert "egress: blocked blocked.example:8080 (1)" in out, f"blocked host not reported: {out}"
        assert "RAW-IP-REACHED" not in out, f"IP filter bypassed: {out}"
        assert "API-REACHED" not in out, f"agentgw API reachable from the sandbox: {out}"
        assert "DBUS-VISIBLE" not in out, f"system bus reachable from the sandbox: {out}"
        assert "API-REACHED-77" not in out, f"wildcard-bound API reachable via the proxy address: {out}"
        assert "NSCD-VISIBLE" not in out, f"nscd (host name resolution) reachable from the sandbox: {out}"
        assert "proxy-env=set" in out and "run-" not in out, f"proxy env missing or token leaked: {out}"
        n = machine.succeed("sqlite3 /var/lib/agentgw/state.db \"select count(*) from audit where event='egress_blocked' and detail='blocked.example:8080'\"").strip()
        assert n == "1", f"egress_blocked audited {n} times"
        out = machine.succeed("sqlite3 /var/lib/agentgw/state.db \"select output from jobs where rule='open-cmd'\"")
        assert "external-ok" in out, f"cmd without egress could not reach the network: {out}"

    with subtest("real claude and codex start in the restricted sandbox and reach only the proxy"):
        assert hook('{"kind":"real","n":300}') == "202"
        try:
            machine.wait_until_succeeds(
                "sqlite3 /var/lib/agentgw/state.db \"select state from jobs where rule='real-clis'\" | grep -qx done",
                timeout=300,
            )
        except Exception:
            dump()
            raise
        out = machine.succeed("sqlite3 /var/lib/agentgw/state.db \"select output from jobs where rule='real-clis'\"")
        print(out)
        assert "claude-version-rc=0" in out and "codex-version-rc=0" in out, f"a CLI failed to start: {out}"
        for crash in ("uv_os_get_passwd", "getpwuid", "No user exists"):
            assert crash not in out, f"user lookup failed without nscd: {out}"
        assert "egress: blocked api.anthropic.com:443" in out, f"claude never reached the proxy: {out}"
        assert "egress: blocked api.openai.com:443" in out, f"codex never reached the proxy: {out}"

    with subtest("stopping agentgw leaves no orphaned action units (cmd and agent)"):
        assert hook('{"kind":"sleep","n":3}') == "202"
        assert hook('{"kind":"agent","n":4}') == "202"
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=60)
        machine.succeed("systemctl stop agentgw.service")
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 0", timeout=30)

    with subtest("after a crash, orphans are stopped before their jobs are requeued"):
        machine.succeed("systemctl start agentgw.service")
        machine.wait_for_open_port(8080)
        # The startup requeue reruns the two interrupted jobs.
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=90)
        before = set(machine.succeed(f"{ACTIVE} | awk '{{print $1}}'").split())
        machine.succeed("systemctl kill -s KILL agentgw.service")
        # Sample through the restart: the agent takes 15 s to stop, so a requeue
        # that didn't wait would show 3-4 instances at once.
        peak = 0
        for _ in range(40):
            peak = max(peak, int(machine.succeed(f"{ACTIVE} | wc -l").strip()))
            machine.sleep(1)
        assert peak <= 2, f"{peak} instances at once: a step ran twice concurrently"
        machine.wait_until_succeeds("systemctl is-active agentgw.service", timeout=60)
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=90)
        after = set(machine.succeed(f"{ACTIVE} | awk '{{print $1}}'").split())
        assert not (before & after), f"orphans still running: {before & after}"
        failed = machine.succeed(f"systemctl list-units --failed --no-legend --plain {UNITS} | wc -l").strip()
        assert failed == "0", f"{failed} failed action units left loaded"
  '';
}
