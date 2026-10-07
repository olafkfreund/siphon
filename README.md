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

Needs Nix with flakes. The example config needs these in the environment
(any values will do for `validate`; `SIPHON_TOKEN` must be 32+ characters):

```sh
export SIPHON_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '=+/')
export FACTORY_TOKEN=x METRICS_API_KEY=x GH_WEBHOOK_SECRET=x PARTNER_WEBHOOK_SECRET=x

nix run github:olafkfreund/siphon -- validate -config examples/siphon.yaml
```

Dry-run a rule against a saved event (nothing is written):

```sh
echo '{"used_pct": 95}' > event.json
nix run github:olafkfreund/siphon -- rules test -config examples/siphon.yaml disk-full event.json
```

Poll every source once and run whatever fires, or run the daemon. Outside the
NixOS module there is no polkit rule, so the systemd sandbox is not available
to an ordinary user; run unsandboxed for a local try-out (not for real use):

```sh
sed 's/sandbox: systemd/sandbox: none/' examples/siphon.yaml > siphon.local.yaml
nix run github:olafkfreund/siphon -- run-once -config siphon.local.yaml
nix run github:olafkfreund/siphon -- serve    -config siphon.local.yaml
```

Header names in `headers[...]` are lower-case for every source type
(`headers["x-github-event"]`).

`serve` listens on `server.listen` (default `:8080`). The portal is at
`http://127.0.0.1:8080/`: log in with the `server.token` value, then browse
jobs, approvals, rules (enable or disable at runtime), sources and the audit
log. `/healthz` needs no login. The JSON API is under `/api/` with
`Authorization: Bearer <token>`.

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

All commands that read the config take `-config <file>` (default `siphon.yaml`).

| Command | What it does |
|---|---|
| `siphon validate [-config f]` | Check the config; prints every problem at once, then `ok`. Warnings go to stderr. |
| `siphon rules test [-config f] <rule> <event.json>` | Dry-run one rule against an event; prints the fires and the rendered argv as JSON. A file shaped `{"headers": {...}, "event": {...}}` supplies request headers (names are lower-cased); any other JSON is the event itself. |
| `siphon run-once [-config f]` | Poll every source once, evaluate rules, run the jobs. Holds the DB lock. |
| `siphon serve [-config f]` | The daemon: pollers, webhook endpoints, workers, portal and API. Holds the DB lock. |
| `siphon jobs ls [-config f] [-state s]` | List jobs (newest first). |
| `siphon credentials import [-config f] [-token-stdin] <name>` | Read a login file (or, with `-token-stdin`, a Claude setup-token) from stdin, check its shape and store it. |
| `siphon credentials ls [-config f]` | List stored logins: name, provider, token expiry, last write-back. Never prints secrets. |
| `siphon approve [-config f] [-by name] <job id>` | Approve a pending job (`-by` defaults to `$USER`). |
| `siphon deny [-config f] [-by name] <job id>` | Deny a pending job. |
| `siphon schema` | Print the JSON Schema for `siphon.yaml`. |
| `siphon version` | Print the version. |

`serve` and `run-once` exclude each other on one DB file; `jobs ls`, `approve`
and `deny` are safe next to a running `serve`.

**On NixOS** the binary is not on `PATH` unless you add it
(`environment.systemPackages = [ config.services.siphon.package ];`), and the
CLI must run as the service user, never as root: a root-run command would create
root-owned SQLite WAL files that the daemon then cannot open. Use the config
file the unit runs with (`systemctl cat siphon` shows its path in `ExecStart`):

```sh
sudo -u siphon siphon jobs ls -config /nix/store/...-siphon.yaml
sudo -u siphon siphon approve -config /nix/store/...-siphon.yaml 42
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
of source, see [`docs/adding-a-source.md`](docs/adding-a-source.md).

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

Agents can also run on local or hosted models through Siphon's own small
agent loop (`kind: model`). It speaks the OpenAI-compatible chat API with
tool calling, offers the agent exactly its `allowed_tools` from its MCP
servers, and runs in the same sandbox, egress restriction and approval flow
as Claude, Codex and agy.

```yaml
server:
  models:
    private_endpoints: ["127.0.0.1:11434"]   # local/LAN endpoints, exact host:port

credentials:
  ollama-local: { provider: ollama, url: "http://127.0.0.1:11434" }
  openrouter:
    provider: openai                       # any OpenAI-compatible API
    url: https://openrouter.ai/api/v1
    api_key: file:/run/agenix/openrouter   # optional; local servers need none

agents:
  triage-local:
    kind: model
    credential: ollama-local
    model: qwen3.8:27b
    prompt: "Task {{.item.id}} failed: {{.item.error}}. Diagnose with the factory tools."
    mcp: [factory]
    allowed_tools: [mcp__factory__task_status]
    max_turns: 10
    timeout: 5m
```

- **The Connections page** (formerly Logins) adds these from the portal:
  - presets for Ollama, LM Studio, OpenRouter, Groq and Mistral, or any
    OpenAI-compatible URL;
  - an optional, write-only API key;
  - **Test connection**, which lists the endpoint's models and its latency.
  - The agent editor suggests models from the chosen connection, and marks
    families known to handle tool calls ("tools ✓": qwen, llama3.x,
    mistral, gpt-oss, command-r).
- **Private endpoints** (loopback or LAN, like most Ollama setups) must be
  listed once in `server.models.private_endpoints`. The portal refuses
  unlisted ones, and link-local and cloud metadata addresses are never
  reachable. On NixOS a local `services.ollama` is added for you through
  `services.siphon.models.privateEndpoints`.
- **Limits:**
  - `max_turns` and `timeout` are enforced.
  - `max_budget_usd` only applies when the endpoint reports a cost
    (OpenRouter does).
  - Models that can't use tools still answer: the loop falls back to a
    plain completion and notes it.

## Services: GitHub and GitLab

The **Services** page connects GitHub and GitLab in a few fields. It creates
ordinary sources (editable afterwards), stores tokens write-only, and shows
a generated webhook secret **once**, with the exact payload URL and the
provider's setup steps. Set `server.public_url` so it can show the full URL.
Webhooks from the internet need Siphon behind a reverse proxy with TLS; for
local testing use `gh webhook forward`.

### GitHub

```yaml
sources:
  github:                                   # agent tools (GitHub-hosted MCP server)
    type: mcp
    url: https://api.githubcopilot.com/mcp/
    auth: { bearer: file:/run/credentials/siphon.service/github-token }
    read: { tool: get_me }
    poll: 24h
  github-hooks:                             # events
    type: webhook
    signature: github
    secret: file:/run/credentials/siphon.service/github-webhook
    id: header.X-GitHub-Delivery
agents:
  pr-reviewer:
    mcp: [github]
    allowed_tools: [mcp__github__get_pull_request, mcp__github__get_pull_request_diff]
```

- **Token:** a **fine-grained** personal access token for only the repos
  agents need, with Contents, Pull requests and Issues set to read and
  Metadata set to read. Add write only if agents should comment or review.
- **Local server:** `package: github` runs the Nix-pinned `github-mcp-server`
  instead (needed for GitHub Enterprise). Its token goes in `env:` and never
  reaches the agent (see below).

### GitLab

```yaml
server:
  services: { private_endpoints: ["gitlab.lan:443"] }   # only for a self-hosted GitLab on your LAN
sources:
  gitlab:                                   # open merge requests, polled
    type: http
    url: "https://gitlab.com/api/v4/projects/group%2Fproject/merge_requests?state=opened"
    headers: { PRIVATE-TOKEN: file:/run/credentials/siphon.service/gitlab-token }
    poll: 5m
  gitlab-hooks:                             # events
    type: webhook
    signature: token
    token_header: X-Gitlab-Token
    secret: file:/run/credentials/siphon.service/gitlab-webhook
    id: header.X-Gitlab-Event-UUID
```

**Token:** a **project access token** with `read_api`.

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
