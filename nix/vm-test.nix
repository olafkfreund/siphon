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
          rules = [
            {
              name = "sandboxed-cmd";
              source = "gh";
              when = ''event.kind == "cmd"'';
              on = "each";
              id = "event.n";
              action.cmd = [ "true" ];
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
          ];
          units = [ "marker.service" ];
        };
      };

      systemd.services.marker = {
        serviceConfig = {
          Type = "oneshot";
          StateDirectory = "marker";
          ExecStart = "${pkgs.coreutils}/bin/touch /var/lib/marker/done";
        };
      };
      systemd.services.not-allowed.serviceConfig = {
        Type = "oneshot";
        ExecStart = "${pkgs.coreutils}/bin/true";
      };

      environment.systemPackages = [
        pkgs.curl
        pkgs.openssl
        pkgs.jq
      ];
    };

  testScript = ''
    import json

    TOKEN = "test-token-0123456789abcdef0123456789"

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

    def jobs():
        out = machine.succeed(f"curl -sf -H 'Authorization: Bearer {TOKEN}' http://127.0.0.1:8080/api/jobs")
        return json.loads(out)

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
        machine.wait_until_succeeds("test -e /var/lib/marker/done", timeout=60)
        machine.wait_until_succeeds(
            f"curl -sf -H 'Authorization: Bearer {TOKEN}' http://127.0.0.1:8080/api/jobs"
            " | jq -e '[.[] | select(.state==\"done\")] | length == 2'",
            timeout=60,
        )

    with subtest("polkit: agentgw cannot start units outside the allowlist"):
        machine.fail("runuser -u agentgw -- systemctl start not-allowed.service")
        machine.fail("runuser -u agentgw -- systemd-run --unit=evil.service true")

    with subtest("stopping agentgw leaves no orphaned action units"):
        assert hook('{"kind":"sleep","n":3}') == "202"
        machine.wait_until_succeeds("systemctl list-units --no-legend 'agentgw-run-*' | grep -q running", timeout=60)
        machine.succeed("systemctl stop agentgw.service")
        machine.wait_until_succeeds(
            "! systemctl list-units --no-legend --state=active,activating 'agentgw-run-*' | grep -q .", timeout=30
        )
  '';
}
