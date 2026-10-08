<img src="docs/brand/mark.svg" width="72" alt="">

# Siphon

**Draws events in. Jets agents out.**

Like an octopus siphon that draws water in and jets it out, Siphon draws events
in from MCP servers, APIs and webhooks and jets out agents, commands and routines.

Formerly agentgw. <!-- legacy-name -->

A self-hosted gateway that watches MCP servers, REST APIs and webhooks, decides
with rules whether something needs attention, and then runs a command, starts a
systemd unit, runs a routine, or hands the problem to an AI agent. One Go
binary is the daemon, the CLI and the web portal. One YAML file is the only
source of truth, and one SQLite file holds the state.

```
sources            rules                        actions
mcp      ─┐        when: <expr>                 cmd      argv, sandboxed
http     ─┼──────▶ on: edge | each      ──────▶ unit     starts an allowlisted systemd unit and waits for it
webhook  ─┘        cooldown, repeat             agent    claude, codex or agy on a subscription login
                                                routine  ordered steps: if, retry, approve, continue_on_error
```

- **Sources** produce events `{source, headers, event}`. `mcp` polls exactly one
  declared read (a resource, or one read-only tool with fixed arguments), `http`
  polls a GET or POST, `webhook` receives `POST /hook/<source>` with an HMAC check.
- **Rules** are [expr](https://expr-lang.org) expressions over `event`, `item`,
  `headers` and `source`. `on: edge` fires on false→true, `on: each` fires once
  per id; `repeat` and `cooldown` limit how often.
- **Actions** run as jobs in a worker pool, with approval, audit and retention.

## Documentation

**Start with the [user guide](docs/README.md).** It's organised by what you
want to do, and each how-to shows the CLI, then how to check it in the
portal, then the YAML.

| | |
|---|---|
| [Getting started](docs/getting-started.md) | 10 minutes: login, connect a model, a webhook task, an agent task |
| [Templates](docs/templates/README.md) | ~30 ready-made tasks: PR review, CI failures, alerts, uptime, AWS, Home Assistant, … |
| [Concepts](docs/concepts.md) | source → rule → action, agents, connections, approvals, sandboxing |
| [Connections](docs/README.md#connections) | models (Ollama, OpenAI-compatible), logins (Claude, Codex, agy), GitHub, GitLab, AWS |
| [Troubleshooting](docs/troubleshooting.md) | `siphon why`, testing against the last event, common errors |
| [CLI reference](docs/cli.md) | every command and flag |
| [For AI assistants](docs/llm.md) · [`AGENTS.md`](AGENTS.md) · [`llms.txt`](llms.txt) | drive Siphon from Claude Code or Codex (`siphon mcp`), or let a model draft tasks (`siphon draft`) |

The portal has the same guide under **Help & Docs**, with a live "get
started" checklist and the template gallery.

## Development

The project ships a [devenv](https://devenv.sh) shell (Go, gopls, sqlite, jq,
curl, openssl) with `GOTOOLCHAIN=local`, so Go never downloads a toolchain:

```sh
devenv shell        # or `devenv allow` once, to activate on cd
run-tests           # go vet + go test -race
schema              # regenerate schema/siphon.schema.json
vm-test             # the NixOS VM test (nix build .#checks.x86_64-linux.vm)
siphon validate -config examples/siphon.yaml
devenv test         # what CI runs: vet + tests; fails on any failing test
```

`nix develop` still works for anyone without devenv.

## Ways to run Siphon

| | How | Isolation | For |
|---|---|---|---|
| **NixOS module** | `services.siphon.enable = true` | **Full**: template units, DynamicUser, a private network per run, egress allowlists, polkit | real deployments on NixOS |
| **microVM** | `nix run github:olafkfreund/siphon#microvm` | **Full** (the same module, in a small NixOS VM); the host stays untouched | trying the real thing, any Linux with KVM |
| **Container image** | `podman run … ghcr.io/olafkfreund/siphon` | **None**: `sandbox: none`, egress not enforced; Siphon says so at start and in the portal | portal, rules, sources and commands on any container host |

**microVM.**
- The first run creates `~/.config/siphon-vm/config/` with a starter
  `siphon.yaml`, a portal token and a webhook secret. Change the location
  with `SIPHON_VM_DIR`.
- The portal opens at **http://127.0.0.1:8090**.
- State (the DB, logins, portal edits) lives in `~/.config/siphon-vm/state.img`
  and survives restarts.
- Edit the config, then restart the VM (Ctrl-C, then run it again).
- From inside the VM your host is `10.0.2.2`, so a host Ollama is
  `10.0.2.2:11434` (add it to `server.models.private_endpoints`).
- `#microvm-unfree` also installs claude-code (a local build only).
- KVM is required.

**Container image.**
```sh
mkdir -p ~/siphon && cd ~/siphon && chmod 700 .   # siphon.yaml, token, secrets: keep them 0600
podman run -d --name siphon --cap-drop=all --read-only --tmpfs /tmp \
  --userns=keep-id:uid=65532,gid=65532 \
  -p 127.0.0.1:8090:8080 -v "$PWD":/etc/siphon:ro,Z -v siphon-state:/var/lib/siphon \
  ghcr.io/olafkfreund/siphon:latest
```
- `--userns=keep-id:uid=65532,gid=65532` maps your user to the image's
  user, so it reads your 0600 files and nobody else on the host can.
  With Docker, `chown 65532` the files instead. Never make them world-readable.
- Set `server.db: /var/lib/siphon/state.db` in the config (the volume).
- `server.sandbox: systemd` is refused inside a container.
- The image has no shell and no agent CLIs. Build your own with
  `nix build --impure --expr '(builtins.getFlake "github:olafkfreund/siphon").lib.x86_64-linux.mkImage { agentPackages = [ … ]; }'`.
- Images for x86_64 and aarch64 are published on version tags.

## Quick start

With Siphon running (see above), from any machine:

```sh
siphon login http://127.0.0.1:8080            # the portal token, at a no-echo prompt
siphon connect model ollama --name ollama-local   # or: siphon connect login claude --setup-token -
siphon template                               # ready-made tasks
siphon template upstream-status > t.yaml && siphon apply -f t.yaml --dry-run && siphon apply -f t.yaml --yes
siphon draft "tell me on ntfy topic my-alerts when GitHub has an incident"   # or let a model write it
siphon status
```

The full walk-through, with real output, is in [Getting started](docs/getting-started.md).
The portal is at the same address: sign in with the `server.token` value.
`/healthz` needs no login, and the JSON API is under `/api/` with
`Authorization: Bearer <token>`.

**Without a running daemon**, the local commands work on a config file:

```sh
nix run github:olafkfreund/siphon -- validate -config examples/siphon.yaml
nix run github:olafkfreund/siphon -- rules test -config examples/siphon.yaml disk-full event.json
```

Outside the NixOS module or microVM there is no systemd sandbox for an
ordinary user. Use `sandbox: none` only for a local try-out.

From a checkout, `nix develop -c go test ./...` runs the tests and
`nix build` builds `./result/bin/siphon`.

## NixOS module

```nix
{
  inputs.siphon.url = "github:olafkfreund/siphon";
  # ...
  imports = [ siphon.nixosModules.default ];

  services.siphon = {
    enable = true;
    # Secrets never go in settings: it ends up world-readable in the Nix store.
    credentials = {
      token = "/run/agenix/siphon-token";
      gh-webhook = "/run/agenix/siphon-gh-webhook";
    };
    settings = {
      server = {
        listen = "127.0.0.1:8080";
        token = "file:/run/credentials/siphon.service/token";
      };
      sources.github = {
        type = "webhook";
        secret = "file:/run/credentials/siphon.service/gh-webhook";
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
    # environmentFile = "/run/agenix/siphon.env";   # alternative: env:NAME references
  };
}
```

The service runs as the static `siphon` user with `StateDirectory`, validates
the config before starting, and installs a polkit rule for action units (see
the security notes). Put a TLS-terminating reverse proxy in front of
`server.listen`.

## CLI

`siphon` is both a **client** of a running Siphon (`login`, `get`, `apply`,
`connect`, `new task`, `test`, `why`, `draft`, `mcp`, …) and a set of
**local** commands that work on a config file (`serve`, `validate`,
`rules test`, `run-once`, `credentials import`). Any command given
`-config` is local. Every command and flag is in the
[CLI reference](docs/cli.md); `siphon help --json` gives the same as data.

On NixOS, `services.siphon.cli.enable` (on by default) puts `siphon` on
PATH and sets `SIPHON_URL` to the local daemon, so `siphon login` is all a
user needs. The URL and token are saved together in a 0600 file the user
owns, and always used as a pair. Plain `http://` is only accepted for
loopback (`--insecure-http` overrides that). Local commands must run as the service user, never root (a
root-run command would create root-owned SQLite WAL files that the daemon
then can't open), against the config the unit runs with (`systemctl cat
siphon` shows its path):

```sh
sudo -u siphon siphon jobs ls -config /nix/store/...-siphon.yaml
```

### Editor support

`schema/siphon.schema.json` is generated from the config structs. Add this as
the first line of your config for completion and validation in editors using
yaml-language-server:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/olafkfreund/siphon/main/schema/siphon.schema.json
```

(Or point `$schema=` at the local `schema/siphon.schema.json`.)

## Configuration

See [`examples/siphon.yaml`](examples/siphon.yaml) for every source type, rule
mode and action type, plus a routine with retry and approval. To add a new kind
of source, see [`docs/developing/adding-a-source.md`](docs/developing/adding-a-source.md).

## Agents and subscriptions

An agent runs a coding-agent CLI inside the same systemd sandbox as `cmd`
actions. **Subscription logins are the default**; API keys stay supported.
Three kinds exist: `claude` (default), `codex` and `agy` (Antigravity).

```yaml
credentials:
  claude-max: { provider: claude }
  chatgpt:    { provider: codex }
  google:     { provider: agy }
  openai-key: { provider: codex, api_key: "file:/run/credentials/siphon.service/openai" }

agents:
  triage:
    kind: codex                  # claude (default) | codex | agy
    credential: chatgpt          # optional if exactly one subscription credential of that kind exists
    command: /run/current-system/sw/bin/codex   # optional binary override
    prompt: "..."
```

### Import a login

Log in on a host where the CLI works, then import the login file as the
service user. The file is read from stdin, so this works across users. The
store is `<dir of server.db>/credentials/<name>/` (mode 0700), which the
sandbox never sees; siphon hands each run only the files it needs.

| Provider | Log in | Import |
|---|---|---|
| claude | `claude` (then `/login`) | `sudo -u siphon siphon credentials import -config <path> claude-max < ~/.claude/.credentials.json` |
| claude, non-rotating (not yet verified live) | `claude setup-token` | `... credentials import -config <path> -token-stdin claude-max` (paste the token on stdin; used as `CLAUDE_CODE_OAUTH_TOKEN`) |
| codex | `codex login` | `sudo -u siphon siphon credentials import -config <path> chatgpt < ~/.codex/auth.json` |
| agy | `agy` (sign in) | `sudo -u siphon siphon credentials import -config <path> google < ~/.gemini/antigravity-cli/antigravity-oauth-token` |

Import checks the file's shape. For Claude only the `claudeAiOauth` object is
kept (MCP OAuth entries are dropped). `siphon credentials ls -config <path>`
shows name, provider, token expiry and last write-back, never a secret.

### Trust: the login lives in the agent's sandbox

A subscription agent has its login, **including the refresh token**, in its own
sandbox HOME while it runs. A prompt-injected agent could try to send it out.
With egress restriction on (the default, see "Egress restriction") it can reach
only the hosts on its allowlist, which narrows that to those hosts but doesn't
eliminate it. Use a dedicated or low-value account for each credential. On write-back siphon re-validates the file's shape, drops unknown
fields, and for codex refuses a login that belongs to a different account
(account id, else the id-token subject; two empty subjects count as different).
That check reads fields the sandboxed CLI controls, so it stops naive swaps
only, not a determined attacker. Claude and agy logins carry no account id to
compare. Each replace keeps the
previous file as `<file>.prev` (0600) in the credential's store directory, so
a bad write-back can be undone by copying it back.

### What each kind can enforce

| Control | claude | codex | agy |
|---|---|---|---|
| Built-in tools off | yes (`--tools ""`) | no: runs read-only (`-s read-only`) | no: plan mode plus `--sandbox` |
| Exact MCP tool allowlist | yes | yes (`enabled_tools`, per-tool approval) | no: only the listed MCP servers are configured (`validate` warns whenever the agent has `mcp:` servers) |
| `max_turns`, `max_budget_usd` | yes | no (timeout only) | no (`--print-timeout` and timeout) |
| Prompt delivery | stdin | stdin | one `--print=<prompt>` argv element |

agy's prompt is therefore on its argv: other users on the host can see it only
under `sandbox: none` (the systemd sandbox hides `/proc`), and prompts over
about 128 KiB fail. MCP bearer headers stay off argv: Codex gets its MCP
configuration from a config file in its private HOME, not from `-c` flags.

`siphon validate` prints one warning per agent for each control its kind
cannot enforce. Nothing is refused: the systemd sandbox remains the outer
boundary. Every kind's result reaches `agent-result` rules as
`{"kind", "result": <final text>, "raw": <the CLI's JSON, or null>}`.

### Refresh, write-back and concurrency

CLIs refresh their tokens while running. After each run siphon copies a
changed login file back into the store, but only if the store still holds the
bytes the run started with (compare-and-swap under a per-credential lock);
otherwise the write-back is dropped (a re-import or another run won) and the
audit log records `credential_writeback_stale`. Write-back bytes never
enter job output, and credential contents are masked in it.
`credentials.<name>.concurrency` (default **1**) limits concurrent runs per
login so two runs never race a rotating refresh token; extra jobs wait.

If a login stops working, the job fails with
`credential <name> needs re-login: log in with <kind> on the host, then run
siphon credentials import <name>`, and the audit log records
`credential_reauth`. Quota errors are reported as
`quota/rate limit reached for <name>` and are not treated as auth failures.
Each refreshed token that is saved back is audited as `credential_refreshed`.

Verified live (2026-10-06) on real subscriptions: a Claude Max login and a
ChatGPT-plan Codex login, each imported with `credentials import`, ran an
agent that called an allowlisted MCP tool. Antigravity authenticated but was
blocked by the account's own quota at the time; its full run is pending.

### API keys

Set `api_key: env:NAME` or `file:/path` on the credential. The key is read
when the config loads, so changing it needs a restart. Claude runs with
`--bare` and receives the key as `ANTHROPIC_API_KEY`; codex gets `{"OPENAI_API_KEY": ...}` in its
`auth.json` with `forced_login_method=api`; agy gets `GEMINI_API_KEY` (not yet
verified against a live agy).

### Where the CLIs come from

Install them into the sandbox with `services.siphon.agentPackages`
(`pkgs.claude-code`, `pkgs.codex`; agy is not in nixpkgs, so use your own
package). System-wide CLI configuration under `/etc` (for example
`/etc/codex/`) is visible inside the sandbox and applies to agent runs; the
user's own `~/.claude`, `~/.codex` and `~/.gemini` are not.

### Subscriptions and terms

Automated or headless use of a consumer subscription may be restricted by the
provider's terms. Read Anthropic's consumer terms of service and usage policy,
OpenAI's terms of use, and Google's terms of service and the Antigravity
terms before pointing a subscription at an unattended agent. Compliance is the
operator's responsibility.

### Migrating from `runner:` and `api_key_file:`

- `runner: [claude, ...]` still works as a deprecated alias for
  `kind: claude` with `command: <runner[0]>`; arguments after the binary are
  ignored, and `validate` warns about both. Replace it with `kind`/`command`.
- `api_key_file: /path` still works: it becomes an implicit API-key credential
  for the agent's kind (named `_apikey_<agent>`, a reserved prefix). Prefer a `credentials:` entry with `api_key: file:/path`.
- `validate` now also rejects three configs that were already wrong:
  credentials in an `http`/`mcp` source URL (`user:pass@host`; use
  `auth.bearer` or `headers`), a config file with more than one YAML
  document, and a `sha256` webhook whose `signature_header` equals its
  `timestamp_header` (case-insensitive).
- Credential names are lower-case `[a-z0-9][a-z0-9_-]*`.
- With `sandbox: none`, agent runs get a private temporary HOME; plain `cmd`
  actions keep yours.
- On NixOS add the CLIs to `services.siphon.agentPackages` so the action
  unit can find them.

## Model connections (Ollama and OpenAI-compatible APIs)

Agents can run on local or hosted models through Siphon's own agent loop
(`kind: model`), in the same sandbox, egress restriction and approval flow
as Claude, Codex and agy. `siphon connect model ollama`, presets for LM
Studio, OpenRouter, Groq and Mistral, private-endpoint rules: see
[docs/connections/models.md](docs/connections/models.md).

## Services: GitHub, GitLab and AWS

The **Services** page connects GitHub, GitLab and AWS in a few fields. It creates
ordinary sources (editable afterwards), stores tokens write-only, and shows
a generated webhook secret **once**, with the exact payload URL and the
provider's setup steps. Set `server.public_url` so it can show the full URL.
Webhooks from the internet need Siphon behind a reverse proxy with TLS; for
local testing use `gh webhook forward`.

### GitHub

`siphon connect github --webhook`: tokens, webhook setup, the tools to allow, and the YAML are in [docs/connections/github.md](docs/connections/github.md).

### GitLab

`siphon connect gitlab --webhook`: tokens, webhook setup, the tools to allow, and the YAML are in [docs/connections/gitlab.md](docs/connections/gitlab.md).

### AWS

Agents get AWS tools through two pinned MCP servers: **CloudWatch** (logs,
metrics, alarms) and **AWS documentation**. They never get a long-lived key.
For each run, Siphon fetches **temporary credentials** and hands them only to
the CloudWatch server's own sandboxed unit (the agent talks to it with a
per-run token).

```nix
services.siphon = {
  aws.enable = true;                        # adds the aws-cloudwatch and aws-docs servers (~1 GB closure)
  aws.configFile = "/etc/siphon/aws-config"; # only for profile/SSO credentials
};
```

```yaml
credentials:
  aws-ro:
    provider: aws
    region: eu-west-1
    profile: siphon-readonly                 # recommended: SSO, credential_process, or role_arn + source_profile
    # or assume a role from a base key Siphon holds (or the host's own role):
    # role_arn: arn:aws:iam::123456789012:role/siphon-readonly
    # external_id: file:/run/credentials/siphon.service/aws-external-id
    # access_key_id: file:/run/credentials/siphon.service/aws-id
    # secret_access_key: file:/run/credentials/siphon.service/aws-secret
sources:
  cloudwatch: { type: mcp, package: aws-cloudwatch, aws: aws-ro }
  aws-docs:   { type: mcp, package: aws-docs }   # public docs, no credentials
agents:
  oncall:
    mcp: [cloudwatch, aws-docs]
    timeout: 30m                             # 55m at most with an AWS source
    allowed_tools: [mcp__cloudwatch__get_active_alarms, mcp__cloudwatch__execute_log_insights_query]
```

- **Read-only role.** Create a dedicated role from
  [`examples/aws/siphon-readonly-policy.json`](examples/aws/siphon-readonly-policy.json)
  (CloudWatch logs and metrics, read only). For `role_arn` use the trust
  policy in [`examples/aws/siphon-trust-policy.json`](examples/aws/siphon-trust-policy.json)
  with a random external ID. Try it in a non-production account first.
  Logs Insights queries are read-only but **billed per GB scanned**. Siphon
  never creates or changes IAM resources.
- **SSO profile.** Put the profile in `aws.configFile` (readable by siphon:
  `chown root:siphon`, `chmod 0640`; sandboxed runs can't read it) and log
  in as the siphon user, whose home is `/var/lib/siphon`:
  `sudo -u siphon HOME=/var/lib/siphon AWS_CONFIG_FILE=/etc/siphon/aws-config aws sso login --profile siphon-readonly`.
  SSO sessions expire (typically 8–12 h); runs then fail with a clear
  message until you log in again. A profile that yields long-lived keys is
  refused.
- **Who may pick an identity.** Credentials in `siphon.yaml` are yours to
  write. Credentials made in the portal (or the Services tile) may only use
  the profiles and role ARNs you list, so a portal user can't switch
  Siphon to a stronger identity its own login can reach:

  ```yaml
  server:
    aws:
      profiles: [siphon-readonly]
      role_arns: [arn:aws:iam::123456789012:role/siphon-readonly]
  ```

  A portal credential that brings its own access keys may assume any role
  those keys can: its power comes from the keys, not from Siphon.
- **Keys must outlive the run.** A profile whose keys would expire before
  the run could finish (a stale SSO login or `credential_process` cache) is
  refused with a message to refresh it.
- **Session length.** Each run gets one session lasting its timeout plus 5
  minutes, between 15 minutes and 1 hour, with no renewal. That is why an
  agent with an AWS source may run 55 minutes at most.
- **Network.** The CloudWatch server reaches only `logs.<region>` and
  `monitoring.<region>.amazonaws.com`; the docs server only
  `docs.aws.amazon.com` and AWS's docs search hosts.
- **Agent tools only.** AWS sources are not polled (yet).
- **Versions.** The servers are pinned to the last releases built on `mcp`
  1.x (CloudWatch 0.1.8, documentation 1.1.30), because nixpkgs ships `mcp`
  1.29. They move up when nixpkgs has `mcp` 2.x.

**EventBridge events in.** Point an EventBridge **API destination** at a
webhook source with `token` authentication:

```yaml
sources:
  aws-events:
    type: webhook
    signature: token
    token_header: X-Siphon-Key
    secret: file:/run/credentials/siphon.service/aws-eventbridge-key
rules:
  - name: alarm
    source: aws-events
    when: 'event["detail-type"] == "CloudWatch Alarm State Change" && event.detail.state.value == "ALARM"'
    on: each
    id: event.id
    action: { agent: oncall }
```

In the EventBridge console, create a **connection** with API key
authorization (key name `X-Siphon-Key`, value the secret), then an **API
destination** (`POST` to `https://<siphon>/hook/aws-events`) and a rule that
targets it. The Services page's AWS tile generates the key and shows the URL.

## Webhook authentication

| `signature:` | Checks | Use for |
|---|---|---|
| `github` | HMAC-SHA256 in `X-Hub-Signature-256` | GitHub |
| `sha256` | hex HMAC-SHA256 in `signature_header`, optional `timestamp_header` | generic providers |
| `standard-webhooks` | HMAC over `webhook-id.webhook-timestamp.body` (`whsec_` secrets), ±5 min, `webhook-id` as the replay key | GitLab signing tokens, Svix, other modern providers |
| `token` | a shared secret in `token_header`, compared in constant time | GitLab `X-Gitlab-Token`, AWS EventBridge API keys |

`token` doesn't protect the body's integrity, so `validate` warns about it.
Prefer an HMAC mode where the provider offers one, and always put TLS in
front.

## MCP servers with secrets

A local (stdio) MCP server that needs a secret gets it from `env:`. The
values are secret references; only `siphon.yaml` decides which variable names
exist, and dangerous names such as `LD_*`, `PATH` and `NODE_OPTIONS` are
refused.

```yaml
server:
  mcp_packages:          # on NixOS: services.siphon.mcpPackages (github-mcp-server by default)
    github: { command: ["/nix/store/…/bin/github-mcp-server", "stdio"], env: [GITHUB_PERSONAL_ACCESS_TOKEN], hosts: ["api.github.com"] }
sources:
  gh-local:
    type: mcp
    package: github
    env: { GITHUB_PERSONAL_ACCESS_TOKEN: file:/run/credentials/siphon.service/github-token }
    read: { tool: get_me }
    poll: 24h
```

- **The MCP bridge keeps the secret away from the agent.** When an agent uses
  such a server, Siphon runs the server in its own sandbox
  (`siphon-mcp@`, under a different user, reaching only the package's
  hosts) and relays only its tools to the agent. The agent never sees the
  secret in its environment, its files or `/proc`.
- **Packages:** the portal may enable only the servers listed in
  `server.mcp_packages`. Any other command can only be set in
  `siphon.yaml`.

## Egress restriction

Agents hold logins and read untrusted data, so their network is restricted
by default. On the NixOS module the sandbox can reach **only siphon's egress
proxy** (an HTTP CONNECT proxy on `server.egress.listen`, default
`127.77.0.1:3128`, loopback only). Each run gets its own proxy credential in
`HTTPS_PROXY` (never argv) and its own allowlist; there is no DNS inside the
sandbox, and the proxy applies the same private-address guard as sources. It
tunnels bytes without inspecting TLS.

On NixOS each restricted run has its own network namespace and an empty
`/run`: `exec-job` forwards `127.0.0.1:3128` inside it to the proxy's unix
socket (`server.egress.socket`, set by the module), so no host port or host
daemon socket (nscd, D-Bus, nix-daemon, ...) is reachable. Don't run
`siphon run-once` alongside `serve` with the same socket path.

An agent's allowlist is the built-in provider hosts for its kind and login type,
plus the host:port of each `mcp:` source with a `url`, plus
`agents.<name>.egress.allow`, plus `server.egress.allow`.

| kind | subscription | API key |
|---|---|---|
| claude | `api.anthropic.com`, `platform.claude.com` | `api.anthropic.com` |
| codex | `chatgpt.com`, `auth.openai.com` | `api.openai.com` |
| agy | `oauth2.googleapis.com`, `daily-cloudcode-pa.googleapis.com`, `cloudcode-pa.googleapis.com`, `www.googleapis.com`, `lh3.googleusercontent.com` | `generativelanguage.googleapis.com` |

Verified 2026-10-06 with each CLI behind a filtering proxy. Provider hosts can
change with a CLI update; a blocked host shows up in the job output
(`egress: blocked <host:port> (N)`) and as an `egress_blocked` audit row, and
you can fix it without a release by adding it to `egress.allow`.

```yaml
server:
  egress:
    allow: []                  # extra hosts for every run: host, host:port or *.suffix
agents:
  triage:
    egress: { allow: [api.github.com] }   # enabled defaults to true; enabled: false opens the network
rules:
  - name: deploy
    egress: { enabled: true, allow: [hooks.example.com] }   # cmd actions are opt-in
```

- `cmd` actions are unrestricted unless the rule sets `egress.enabled` (they
  then get only their listed hosts) or `server.egress.cmd_default: true`.
- Allowed ports: `host:443`, or the exact `host:port` of an allowed MCP URL or
  allow entry. `*.suffix` matches subdomains only.
- `siphon validate -v` prints each agent's and each egress-enabled rule's
  effective list.
- **Residual risk:** this narrows exfiltration to the allowed hosts; it does
  not eliminate it, because a prompt-injected agent can still abuse an allowed
  provider or operator endpoint.
- With `sandbox: none` nothing is enforced (`validate` warns); the proxy
  variables are still set, so cooperative CLIs are filtered anyway.
- Turn it off with `services.siphon.egress.enable = false` on NixOS, or per
  agent with `egress: { enabled: false }`. With the module option off, runs
  still get the proxy variables but nothing enforces them (the sandbox keeps
  only the metadata-address deny).

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
  defines a template unit, `siphon-action@.service`: DynamicUser,
  ProtectSystem=strict, ProtectHome, PrivateTmp, NoNewPrivileges, an empty
  capability set, a syscall filter, hidden `/proc`, the metadata addresses
  blocked, `LimitFSIZE=16M`, `TasksMax` and a `maxActionRuntime` ceiling. An
  action cannot read siphon's state DB or credentials (the VM test checks).
- **Secrets stay off argv and out of readable paths, with one exception.**
  siphon hands each run its argv, stdin and private files (MCP config, API key,
  subscription login) in a job file that only siphon and that run's sandbox
  user (via group `siphon-io`) can read; inside the unit they land in its
  private `/tmp`. The exception is deliberate: **a subscription agent holds its
  login's refresh token in its own sandbox HOME**, and a prompt-injected agent
  could try to exfiltrate it. Egress restriction limits it to the allowlisted
  hosts (see "Egress restriction"); use a dedicated or low-value account per
  credential. agy also takes its prompt as an argv element.
- **siphon cannot raise its own privileges.** The module's polkit rule lets the
  `siphon` user only start/stop/reset `siphon-action@<16 hex>` instances and
  start the units in your `units:` allowlist; every other polkit action
  (transient units, enable, daemon-reload, set-property, …) is refused. systemd
  itself never opens a file in a directory siphon can write (the unit creates
  its own outputs with `O_NOFOLLOW`), so a planted symlink cannot make root
  write anywhere. The NixOS VM test checks each of these.
- **No orphaned or doubled actions.** Stopping siphon stops its running
  instances and waits for them; after a crash, the next start stops leftover
  instances and waits before requeueing their jobs, so a step never runs twice
  at once (the VM test samples this through a kill and restart).
- **A timed-out or cancelled unit action leaves the unit running.** Unlike a
  `cmd` or agent, an allowlisted unit is an operator-owned service: siphon
  stops waiting for it but does not stop it.
- **`sandbox: systemd` needs the NixOS module** (it provides the template
  unit, the `siphon-io` group, `/var/lib/siphon-actions` and the polkit
  rule). Elsewhere, install equivalents yourself or use `sandbox: none`.
  With `sandbox: none` under the module, actions run as the `siphon` user
  itself, with its access and polkit grants: don't combine the two.
- **Agents reach only their allowlist; `cmd` actions can reach the network**
  unless a rule opts in to egress restriction (see "Egress restriction"), so
  keep local services authenticated. With restriction off, a run also reaches
  local services and siphon's own API, apart from cloud metadata addresses.
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
  through the API, or with `siphon approve <id>`. Approval tokens are stored
  only as hashes and are never logged. Every decision is audited.
- **Secrets** are only `env:NAME` or `file:/path` references; inline secrets are
  rejected at load. Known secret values are masked as `***` in stored output and
  logs.

## Credits and licence

Apache License 2.0, see [LICENSE](LICENSE).

The design learned from [Windmill](https://www.windmill.dev) (job queue,
sandboxing and SSRF lessons). No Windmill code was copied.

## Upgrading from agentgw <!-- legacy-name -->

- `services.agentgw` still works, with a warning, until v0.2.0. <!-- legacy-name -->
- On first start, state is copied from `/var/lib/agentgw` to `/var/lib/siphon`; the old copy is kept. <!-- legacy-name -->
- `agentgw.yaml` is still read if `siphon.yaml` is missing. <!-- legacy-name -->
- The `agentgw` binary is a symlink to `siphon` and prints a deprecation notice. <!-- legacy-name -->
- Without a `db:` setting, an existing `agentgw.db` next to the config is still used (with a warning) until you rename it to `siphon.db`. <!-- legacy-name -->
- If you set `settings.server.db` to a path under `/var/lib/agentgw`, change it to `/var/lib/siphon`: only the default state is migrated. <!-- legacy-name -->
