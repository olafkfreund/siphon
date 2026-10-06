# agentgw

A self-hosted gateway that watches MCP servers, REST APIs and webhooks, decides
with rules whether something needs attention, and then runs a command, starts a
systemd unit, runs a routine, or hands the problem to an AI agent. One Go
binary is the daemon, the CLI and the web portal. One YAML file is the only
source of truth, and one SQLite file holds the state.

```
sources            rules                        actions
mcp      ─┐        when: <expr>                 cmd      argv, sandboxed
http     ─┼──────▶ on: edge | each      ──────▶ unit     starts an allowlisted systemd unit and waits for it
webhook  ─┘        cooldown, repeat             agent    claude (or any argv runner), tool allowlist
                                                routine  ordered steps: if, retry, approve, continue_on_error
```

- **Sources** produce events `{source, headers, event}`. `mcp` polls exactly one
  declared read (a resource, or one read-only tool with fixed arguments), `http`
  polls a GET or POST, `webhook` receives `POST /hook/<source>` with an HMAC check.
- **Rules** are [expr](https://expr-lang.org) expressions over `event`, `item`,
  `headers` and `source`. `on: edge` fires on false→true, `on: each` fires once
  per id; `repeat` and `cooldown` limit how often.
- **Actions** run as jobs in a worker pool, with approval, audit and retention.

## Quick start

Needs Nix with flakes. The example config needs these in the environment
(any values will do for `validate`; `AGENTGW_TOKEN` must be 32+ characters):

```sh
export AGENTGW_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '=+/')
export FACTORY_TOKEN=x METRICS_API_KEY=x GH_WEBHOOK_SECRET=x PARTNER_WEBHOOK_SECRET=x

nix run github:olafkfreund/MCP-AgentGateway -- validate -config examples/agentgw.yaml
```

Dry-run a rule against a saved event (nothing is written):

```sh
echo '{"used_pct": 95}' > event.json
nix run github:olafkfreund/MCP-AgentGateway -- rules test -config examples/agentgw.yaml disk-full event.json
```

Poll every source once and run whatever fires, or run the daemon. Outside the
NixOS module there is no polkit rule, so the systemd sandbox is not available
to an ordinary user; run unsandboxed for a local try-out (not for real use):

```sh
sed 's/sandbox: systemd/sandbox: none/' examples/agentgw.yaml > agentgw.local.yaml
nix run github:olafkfreund/MCP-AgentGateway -- run-once -config agentgw.local.yaml
nix run github:olafkfreund/MCP-AgentGateway -- serve    -config agentgw.local.yaml
```

Header names in `headers[...]` are lower-case for every source type
(`headers["x-github-event"]`).

`serve` listens on `server.listen` (default `:8080`). The portal is at
`http://127.0.0.1:8080/`: log in with the `server.token` value, then browse
jobs, approvals, rules (enable or disable at runtime), sources and the audit
log. `/healthz` needs no login. The JSON API is under `/api/` with
`Authorization: Bearer <token>`.

From a checkout, `nix develop -c go test ./...` runs the tests and
`nix build` builds `./result/bin/agentgw`.

## NixOS module

```nix
{
  inputs.agentgw.url = "github:olafkfreund/MCP-AgentGateway";
  # ...
  imports = [ agentgw.nixosModules.default ];

  services.agentgw = {
    enable = true;
    # Secrets never go in settings: it ends up world-readable in the Nix store.
    credentials = {
      token = "/run/agenix/agentgw-token";
      gh-webhook = "/run/agenix/agentgw-gh-webhook";
    };
    settings = {
      server = {
        listen = "127.0.0.1:8080";
        token = "file:/run/credentials/agentgw.service/token";
      };
      sources.github = {
        type = "webhook";
        secret = "file:/run/credentials/agentgw.service/gh-webhook";
        signature = "github";
      };
      rules = [{
        name = "pr-opened";
        source = "github";
        when = ''headers["x-github-event"] == "pull_request"'';
        on = "each";
        id = "event.number";
        action.cmd = [ "echo" "PR {{.event.number}}" ];
      }];
      units = [ "nix-gc.service" ];
    };
    # environmentFile = "/run/agenix/agentgw.env";   # alternative: env:NAME references
  };
}
```

The service runs as the static `agentgw` user with `StateDirectory`, validates
the config before starting, and installs a polkit rule for action units (see
the security notes). Put a TLS-terminating reverse proxy in front of
`server.listen`.

## CLI

All commands that read the config take `-config <file>` (default `agentgw.yaml`).

| Command | What it does |
|---|---|
| `agentgw validate [-config f]` | Check the config; prints every problem at once, then `ok`. Warnings go to stderr. |
| `agentgw rules test [-config f] <rule> <event.json>` | Dry-run one rule against an event; prints the fires and the rendered argv as JSON. |
| `agentgw run-once [-config f]` | Poll every source once, evaluate rules, run the jobs. Holds the DB lock. |
| `agentgw serve [-config f]` | The daemon: pollers, webhook endpoints, workers, portal and API. Holds the DB lock. |
| `agentgw jobs ls [-config f] [-state s]` | List jobs (newest first). |
| `agentgw approve [-config f] [-by name] <job id>` | Approve a pending job (`-by` defaults to `$USER`). |
| `agentgw deny [-config f] [-by name] <job id>` | Deny a pending job. |
| `agentgw schema` | Print the JSON Schema for `agentgw.yaml`. |
| `agentgw version` | Print the version. |

`serve` and `run-once` exclude each other on one DB file; `jobs ls`, `approve`
and `deny` are safe next to a running `serve`.

**On NixOS** the binary is not on `PATH` unless you add it
(`environment.systemPackages = [ config.services.agentgw.package ];`), and the
CLI must run as the service user, never as root: a root-run command would create
root-owned SQLite WAL files that the daemon then cannot open. Use the config
file the unit runs with (`systemctl cat agentgw` shows its path in `ExecStart`):

```sh
sudo -u agentgw agentgw jobs ls -config /nix/store/...-agentgw.yaml
sudo -u agentgw agentgw approve -config /nix/store/...-agentgw.yaml 42
```

### Editor support

`schema/agentgw.schema.json` is generated from the config structs. Add this as
the first line of your config for completion and validation in editors using
yaml-language-server:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/olafkfreund/MCP-AgentGateway/main/schema/agentgw.schema.json
```

(Or point `$schema=` at the local `schema/agentgw.schema.json`.)

## Configuration

See [`examples/agentgw.yaml`](examples/agentgw.yaml) for every source type, rule
mode and action type, plus a routine with retry and approval. To add a new kind
of source, see [`docs/adding-a-source.md`](docs/adding-a-source.md).

## Security notes

Read these before running it anywhere that matters.

- **Upstream data is untrusted.** Anything from a source (MCP results, API
  responses, webhook bodies) can be attacker-influenced. Rules and templates
  see it; treat prompts built from it as prompt-injection surface.
- **Agents are fenced in.** The prompt goes to the runner on stdin, never argv.
  The runner gets an exact tool allowlist (`allowed_tools`), `--tools ""` to
  disable built-in tools, turn and budget limits, and only the MCP sources named
  in the agent's `mcp:` list. `approve` defaults to **true** for agents, and a
  `cooldown` is mandatory on any rule that can start one. A daily cap
  (`limits.agent_runs_per_day`) and a depth limit of 2 stop agent-to-agent
  loops; agent results only reach rules that set `allow_agent_events: true`.
- **No shell, ever.** `cmd` is an argv array, rendered element by element.
  `argv[0]` can never be templated, and a templated element may not start with
  `-`. Unit names in `units:` cannot be templated either.
- **Every `cmd` and agent runs in a fixed systemd sandbox.** The NixOS module
  defines a template unit, `agentgw-action@.service`: DynamicUser,
  ProtectSystem=strict, ProtectHome, PrivateTmp, NoNewPrivileges, an empty
  capability set, a syscall filter, hidden `/proc`, the metadata addresses
  blocked, `LimitFSIZE=16M`, `TasksMax` and a `maxActionRuntime` ceiling. An
  action cannot read agentgw's state DB or credentials (the VM test checks).
- **Secrets stay off argv and out of readable paths.** agentgw hands each run
  its argv, stdin and private files (MCP config, API key) in a job file that
  only agentgw and that run's sandbox user (via group `agentgw-io`) can read;
  inside the unit they land in its private `/tmp`.
- **agentgw cannot raise its own privileges.** The module's polkit rule lets the
  `agentgw` user only start/stop/reset `agentgw-action@<16 hex>` instances and
  start the units in your `units:` allowlist; every other polkit action
  (transient units, enable, daemon-reload, set-property, …) is refused. systemd
  itself never opens a file in a directory agentgw can write (the unit creates
  its own outputs with `O_NOFOLLOW`), so a planted symlink cannot make root
  write anywhere. The NixOS VM test checks each of these.
- **No orphaned or doubled actions.** Stopping agentgw stops its running
  instances and waits for them; after a crash, the next start stops leftover
  instances and waits before requeueing their jobs, so a step never runs twice
  at once (the VM test samples this through a kill and restart).
- **A timed-out or cancelled unit action leaves the unit running.** Unlike a
  `cmd` or agent, an allowlisted unit is an operator-owned service: agentgw
  stops waiting for it but does not stop it.
- **`sandbox: systemd` needs the NixOS module** (it provides the template
  unit, the `agentgw-io` group, `/var/lib/agentgw-actions` and the polkit
  rule). Elsewhere, install equivalents yourself or use `sandbox: none`.
  With `sandbox: none` under the module, actions run as the `agentgw` user
  itself, with its access and polkit grants: don't combine the two.
- **Actions can reach the network**, including local services (and agentgw's
  own API on localhost), apart from cloud metadata addresses. Keep local
  services authenticated.
- **Outbound requests are guarded.** `http` and `mcp` sources reject loopback,
  private, link-local, CGNAT and cloud-metadata addresses after DNS resolution
  unless the source sets `allow_private: true`. No redirects; body size and time
  are capped.
- **Webhooks.** HMAC is checked in constant time. The replay key is a hash of
  the signed body, so a captured request cannot be replayed with a fresh
  delivery id. Without a signed timestamp, byte-identical bodies are rejected for
  7 days (so a provider that legitimately re-sends identical payloads needs a
  timestamp or a unique field in the body). Webhook endpoints are rate limited.
- **Portal and API.** One token (`server.token`, 32+ characters, `env:` or
  `file:` only). Sessions are HMAC-signed cookies (HttpOnly, SameSite=Strict)
  that last 24 hours and survive logout until they expire or the daemon
  restarts; POSTs need a CSRF token. The portal is plain HTTP: terminate TLS in
  front of it.
- **Auth rate limit.** Failed logins and API auth failures are limited per
  connecting IP (per /64 for IPv6). Behind a reverse proxy every client shares
  the proxy's address, so they share one limit.
- **Approvals.** There are no one-shot approval links. Approve in the portal,
  through the API, or with `agentgw approve <id>`. Approval tokens are stored
  only as hashes and are never logged. Every decision is audited.
- **Secrets** are only `env:NAME` or `file:/path` references; inline secrets are
  rejected at load. Known secret values are masked as `***` in stored output and
  logs.

## Credits and licence

Apache License 2.0, see [LICENSE](LICENSE).

The design learned from [Windmill](https://www.windmill.dev) (job queue,
sandboxing and SSRF lessons). No Windmill code was copied.
