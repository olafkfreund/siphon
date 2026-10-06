{ self, pkgs }:

pkgs.testers.runNixOSTest {
  name = "agentgw";

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
            listen = "127.0.0.1:8080";
            token = "file:/run/credentials/agentgw.service/token";
          };
          sources.gh = {
            type = "webhook";
            secret = "file:/run/credentials/agentgw.service/hook";
            signature = "github";
          };
          # A stand-in agent runner: same template unit as a real `claude` run.
          agents.slow = {
            runner = [
              "sh"
              "-c"
              "sleep 600"
              "agent"
            ];
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

      environment.systemPackages = [
        pkgs.sqlite
        pkgs.curl
        pkgs.openssl
        pkgs.jq
      ];
    };

  testScript = ''
    TOKEN = "test-token-0123456789abcdef0123456789"
    ACTIVE = "systemctl list-units --no-legend --plain --state=active,activating 'agentgw-action@*'"

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
        print(machine.execute("journalctl -u agentgw -u 'agentgw-action@*' -u marker -u polkit --no-pager | tail -80")[1])

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

    with subtest("polkit: only agentgw-action@<hex> and the allowlist"):
        denied_by_rule("systemctl start not-allowed.service")
        denied_by_rule("systemctl restart marker.service")
        denied_by_rule("systemctl start agentgw-action@not-hex.service")
        denied_by_rule("systemd-run --unit=evil.service true")
        # A transient unit borrowing the template's name, asking for root: must not run.
        machine.fail("runuser -u agentgw -- systemd-run --wait -p User=root --unit=agentgw-action@0123456789abcdef.service touch /root/pwned")
        machine.fail("test -e /root/pwned")

    with subtest("stopping agentgw leaves no orphaned action units (cmd and agent)"):
        assert hook('{"kind":"sleep","n":3}') == "202"
        assert hook('{"kind":"agent","n":4}') == "202"
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=60)
        machine.succeed("systemctl stop agentgw.service")
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 0", timeout=30)

    with subtest("after a crash, the orphan is stopped before its job is requeued"):
        machine.succeed("systemctl start agentgw.service")
        machine.wait_for_open_port(8080)
        # The startup requeue reruns the two interrupted jobs.
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=60)
        before = set(machine.succeed(f"{ACTIVE} | awk '{{print $1}}'").split())
        machine.succeed("systemctl kill -s KILL agentgw.service")
        machine.wait_until_succeeds("systemctl is-active agentgw.service", timeout=60)
        for unit in before:
            machine.wait_until_fails(f"systemctl is-active {unit}", timeout=60)
        # Requeued again in fresh instances; never two copies at once.
        machine.wait_until_succeeds(f"test $({ACTIVE} | wc -l) -eq 2", timeout=60)
        after = set(machine.succeed(f"{ACTIVE} | awk '{{print $1}}'").split())
        assert not (before & after), f"orphans still running: {before & after}"
  '';
}
