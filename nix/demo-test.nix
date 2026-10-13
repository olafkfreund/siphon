# checks.demo: `demo-play --auto` end to end on one NixOS node, with the demo's
# siphon.yaml, the stub model/MCP server and the notification log (Plan step 8).
{ self, pkgs }:

let
  system = pkgs.stdenv.hostPlatform.system;
  demoPlay = self.packages.${system}.demo-play;
  # The demo wrapper fills this in at every start; here it is always the stub.
  siphonYaml = builtins.replaceStrings [ "@MODEL_URL@" ] [ "http://127.0.0.1:8000/v1" ] (
    builtins.readFile ../demo/siphon.yaml
  );
  # Test-only secrets; real runs get random ones from the demo wrapper.
  secrets = {
    token = "demo-test-token-0123456789abcdef0123";
    hook = "demo-test-hook-secret-0123456789";
    kuma = "demo-test-kuma-secret-0123456789";
  };
in
pkgs.testers.runNixOSTest {
  name = "siphon-demo";

  nodes.machine =
    { pkgs, ... }:
    {
      imports = [ self.nixosModules.default ];
      environment.etc =
        {
          "siphon/siphon.yaml".text = siphonYaml;
          "siphon/notify-url".text = "http://127.0.0.1:18099/n";
        }
        // builtins.mapAttrs (_: text: { inherit text; }) (
          builtins.listToAttrs (
            map (n: {
              name = "siphon/${n}";
              value = secrets.${n};
            }) (builtins.attrNames secrets)
          )
        );
      services.siphon = {
        enable = true;
        configFile = "/etc/siphon/siphon.yaml";
      };
      systemd.services.stub-model = {
        wantedBy = [ "multi-user.target" ];
        before = [ "siphon.service" ];
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 ${../nix/stub-model.py} ${../demo/fixtures/ci-job.log}";
      };
      systemd.services.notify-recv = {
        wantedBy = [ "multi-user.target" ];
        before = [ "siphon.service" ];
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 ${../nix/notify-recv.py}";
      };
      environment.systemPackages = [
        demoPlay
        pkgs.python3
        self.packages.${system}.siphon
      ];
    };

  testScript = ''
    import json

    TOKENS = ${builtins.toJSON (builtins.attrValues secrets)}
    machine.start()
    machine.wait_for_unit("siphon.service")
    machine.wait_for_open_port(8080)
    machine.wait_for_open_port(8000)
    machine.wait_for_open_port(18099)

    # The directory layout the wrapper makes: config/{token,hook,kuma}, mode 0600.
    machine.succeed("mkdir -p /root/demo/config && chmod 700 /root/demo")
    for n in ["token", "hook", "kuma"]:
        machine.succeed(f"install -m 600 /etc/siphon/{n} /root/demo/config/{n}")

    # A decoy CLI login: demo-play must ignore it and talk to the demo only.
    env = "SIPHON_DEMO_DIR=/root/demo SIPHON_DEMO_URL=http://127.0.0.1:8080 SIPHON_URL=http://127.0.0.1:9 SIPHON_TOKEN_FILE=/nonexistent"
    cli = "SIPHON_URL=http://127.0.0.1:8080 SIPHON_TOKEN_FILE=/root/demo/config/token siphon"
    # --auto approves at once, before the notifier's 15 s scan could push the
    # approval, so this plays the presenter's way instead: demo-play waits for
    # an approval made elsewhere (the portal; here the CLI, once the "phone"
    # has been told) and Enter comes from printf.
    approver = f"""
    for i in $(seq 1 90); do
      grep -q '"event": *"approval"' /tmp/notify.log 2>/dev/null && break
      sleep 2
    done
    id=$({cli} jobs ls -state pending_approval -o json | python3 -I -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
    {cli} approve "$id"
    """
    machine.succeed(f"cat > /tmp/approver.sh <<'EOS'\n{approver}\nEOS")
    machine.succeed("bash /tmp/approver.sh >/tmp/approver.log 2>&1 &")
    machine.succeed(f"(printf 'y\\ny\\n' | timeout 400 env {env} demo-play >/tmp/play.log 2>&1; echo rc=$? >>/tmp/play.log) >/dev/null 2>&1 &")
    try:
        machine.wait_until_succeeds("grep -q '^rc=' /tmp/play.log", timeout=300)
    finally:
        out = machine.succeed("cat /tmp/play.log; echo --- approver; cat /tmp/approver.log; echo --- notify; cat /tmp/notify.log")
        print(out)
    assert "rc=0" in out, out

    jobs = json.loads(machine.succeed(f"{cli} jobs ls -o json"))
    def done(rule):
        return [j for j in jobs if j["rule"] == rule and j["state"] == "done"]
    ci = done("ci-failed")
    assert len(ci) == 1, jobs
    o = json.loads(machine.succeed(f"{cli} jobs show {ci[0]['id']} -o json"))["output"]
    assert "TestTotalRounding" in o and "go test ./..." in o, o
    assert len(done("monitor-down")) == 1, jobs
    assert len(done("morning-digest")) == 1, jobs

    machine.succeed("grep -q 'tool=get_job_logs' /tmp/mcp-calls")
    lines = [json.loads(l) for l in machine.succeed("cat /tmp/notify.log").splitlines() if l.strip()]
    assert len([l for l in lines if l.get("event") == "approval"]) == 1, lines
    assert len([l for l in lines if l.get("path") == "/kuma"]) == 1, lines
    assert len([l for l in lines if l.get("path") == "/digest"]) == 1, lines

    # No secret in the play output or in the journal.
    journal = machine.succeed("journalctl --no-pager")
    for t in TOKENS:
        assert t not in out and t not in journal, "a demo secret leaked"
  '';
}
