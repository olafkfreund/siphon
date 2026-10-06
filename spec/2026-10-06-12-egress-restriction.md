---
status: approved
issue: 12
intent: intent/2026-10-06-12-egress-restriction.md
---

# Spec: Built-in egress restriction for sandboxed agents and actions

## Evidence (live, 2026-10-06, through a logging and filtering CONNECT proxy)

| CLI | Honours `HTTPS_PROXY` | Sends `user:pass@` from the proxy URL as `Proxy-Authorization` | Minimal working host set (verified) | Seen but not needed |
|---|---|---|---|---|
| claude | yes | yes | API key: `api.anthropic.com`. Subscription: also `platform.claude.com` (token refresh, `/v1/oauth/token`, taken from the binary; refresh was not triggered live, to protect the owner's session) | `http-intake.logs.us5.datadoghq.com` (telemetry) |
| codex | yes | yes | Subscription: `chatgpt.com` alone (a full run returned "OK" with every other host refused). API key: `api.openai.com`; refresh: `auth.openai.com` | `ab.chatgpt.com`, `*.oaiusercontent.com` |
| agy | yes | yes | `oauth2.googleapis.com`, `daily-cloudcode-pa.googleapis.com`, `www.googleapis.com`, `lh3.googleusercontent.com`. The last is **required**: agy's "eligibility check" fetches the profile picture and fails without it. Verified: with these four the run reaches the model API (the quota error) | `antigravity-unleash.goog`, `play.googleapis.com` |

## Design

### Enforcement: the sandbox can only reach agentgw's egress proxy

- agentgw serves a small **HTTP CONNECT proxy** (stdlib) on a dedicated
  loopback address, by default **`127.77.0.1:3128`**. It is a separate
  listener from the API/portal.
- **Template unit (NixOS module), when egress is enforced:**
  `IPAddressDeny=any` and `IPAddressAllow=127.77.0.1/32`.
  - The sandbox can open TCP connections only to the proxy. Everything else
    is refused by systemd's eBPF IP filter: raw IPs, other loopback services
    such as agentgw's API on 127.0.0.1, DNS servers, and the internet.
  - An agent that ignores `HTTPS_PROXY` simply gets no network.
  - This replaces the metadata-only `IPAddressDeny`.
- **No DNS inside the sandbox.** The proxy resolves CONNECT host names, and
  then applies the existing SSRF guard (private, link-local and metadata
  addresses are refused unless the host is an explicitly allowed private
  MCP source).
- **No TLS interception.** CONNECT tunnels the bytes untouched; the proxy only
  sees `host:port`.

### Per-run identity and allowlist

- **Per-run credential:** for each sandboxed run that has egress enabled,
  agentgw makes a random 32-byte token and passes
  `HTTPS_PROXY=HTTP_PROXY=http://run-<id>:<token>@127.77.0.1:3128` (plus the
  lower-case forms) in the job env, never in argv. `NO_PROXY` is cleared.
- **Lookup and expiry:** the proxy looks up the run by `Proxy-Authorization`
  and allows `CONNECT host:443`, plus the explicit `host:port` of an allowed
  MCP URL, only if the host matches that run's allowlist. Exact host names;
  a leading `*.` matches subdomains only.
  - Missing or bad credentials get 407. A host that isn't allowed gets 403.
  - The token expires when the run ends.
- **The effective allowlist for an agent run** is the union of:
  - the **built-in provider hosts** for its `kind` and auth mode (table below)
  - the **host:port of each MCP source** in `agents.<name>.mcp` that has a
    `url` (stdio servers spawned inside the sandbox get the same proxy env, so
    their own outbound calls are filtered by the same list)
  - `agents.<name>.egress.allow` (extra hosts)
  - `server.egress.allow` (global extras, the emergency override)

| kind | subscription | api_key |
|---|---|---|
| claude | `api.anthropic.com`, `platform.claude.com` | `api.anthropic.com` |
| codex | `chatgpt.com`, `auth.openai.com` | `api.openai.com` |
| agy | `oauth2.googleapis.com`, `daily-cloudcode-pa.googleapis.com`, `cloudcode-pa.googleapis.com`, `www.googleapis.com`, `lh3.googleusercontent.com` | `generativelanguage.googleapis.com` (if agy's API-key mode is ever verified) |

- **Claude:** agentgw also sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`,
  so it doesn't keep retrying telemetry against the proxy.

### Config

```yaml
server:
  egress:
    listen: 127.77.0.1:3128   # default; the module's IPAddressAllow must match
    allow: []                 # global extra hosts
agents:
  triage:
    egress: { allow: [api.github.com], enabled: true }   # enabled defaults to true
rules:
  - name: deploy
    action: { cmd: [...] }
    egress: { enabled: true, allow: [hooks.example.com] }  # cmd actions: opt-in
```

- **Agents:** `egress.enabled` defaults to `true`.
- **`cmd` actions:** egress is off unless `rules[].egress.enabled` or
  `server.egress.cmd_default: true`. A `cmd` with egress gets only the hosts
  it lists.
- **Routine steps:** each step inherits from its own action kind (agent
  steps restricted, cmd steps per the rule).
- **`sandbox: none`:** egress can't be enforced. `validate` warns once per
  agent; the env is still set, so cooperative CLIs are filtered anyway.

### NixOS module

- `services.agentgw.egress.enable` (default `true`) sets the template's
  `IPAddressDeny=any` and `IPAddressAllow=127.77.0.1/32`, and
  `server.egress.listen` defaults to match.
- **Runs without egress** (a `cmd` with egress off) need open network. The
  template can't vary per run, so there are **two templates**: a restricted
  `agentgw-action@` and an open `agentgw-action-open@`.
  - The polkit rule allows both name patterns.
  - agentgw picks the template per run.
- `validate` errors if the configured listen address isn't loopback.

### Visibility

- **`validate`** prints each agent's effective allowlist, and with
  `-v`, every rule's.
- **Blocked attempts** are counted per run. The job output gets a final line
  `egress: blocked <host:port> (N times)`, and the audit log records
  `egress_blocked`, rate-limited to one row per host per run. These names
  are what to add to `egress.allow` for a legitimate miss.

## Alternatives rejected

- **IP allowlists of provider hosts (`IPAddressAllow` with resolved IPs):**
  CDNs rotate their IPs, and a resolved snapshot breaks within hours.
- **A transparent proxy with SNI inspection (iptables/nftables redirect):** it
  needs root-owned firewall rules, works per host rather than per run, and is
  more machinery. CONNECT with per-run credentials is simpler and all three
  CLIs support it (verified).
- **TLS interception / MITM:** the intent rules it out, and it would break
  certificate pinning.
- **`PrivateNetwork=yes` plus a socket-activated bridge:** much more
  complexity than an IP allowlist of one loopback address.
- **One shared allowlist for all runs:** the intent chose per-agent.

## Risks

| Risk | Mitigation |
|---|---|
| A provider adds a required host and agents break after a CLI update | Blocked hosts appear clearly in job output and audit. `egress.allow` (per agent or global) fixes it without a release. The README lists the verified hosts with dates |
| An agent abuses an allowed host for exfiltration (for example a provider API) | Accepted residual risk: allowed hosts are provider or operator endpoints. The README says the restriction narrows exfiltration to allowed hosts and doesn't eliminate it |
| A stdio MCP server needs hosts the agent's list lacks | Its blocked host shows up in the output; add it via `egress.allow` |
| The 127.77.0.1 listener collides with something on the host | Configurable listen address, with `validate` and the module kept consistent |
| `systemd` IP filtering needs cgroup v2 + BPF | NixOS defaults have both. The VM test proves it, and `validate` warns if the module option is off |
| Non-HTTP traffic (raw TCP, UDP) | Blocked entirely by `IPAddressDeny=any`; only CONNECT through the proxy works |

## Verification

- **Unit tests:**
  - **Proxy:** allowed host passes, other host gets 403, bad or missing
    credentials get 407, an expired run token gets 407, a non-443 port is
    refused unless it comes from an MCP URL, and `*.` matching behaves.
  - **SSRF guard:** an allowed name resolving to a private IP is refused
    unless `allow_private`.
  - **Allowlist assembly:** per kind, auth mode, MCP sources and extras.
  - **Env injection:** the proxy URL is in env, never in argv.
  - **`validate`:** prints the allowlist and gives the `sandbox: none`
    warning.
- **NixOS VM test**, inside the restricted template:
  - a stand-in agent reaches an allowed host through the proxy;
  - a disallowed host gets 403 and shows up in the job output and audit;
  - a raw IP connection, ignoring the proxy, is refused by the IP filter;
  - loopback services other than the proxy (agentgw's API) are unreachable;
  - a `cmd` without egress uses the open template and reaches the test host.
  - The VM serves a local "external" test host on a second VM or network
    namespace, since the sandbox has no internet.
- **Live:** one claude (API-key or subscription, whichever is safe at the
  time) and one codex subscription run through the real proxy, with the
  built-in lists only. agy once its quota resets.

## Amendment 1 (status: draft, 2026-10-06): private network namespace for the restricted template

**Why.** The pre-PR security review found that `IPAddressAllow=127.77.0.1/32` filters by address, not by port.

- Any host service bound to all addresses is reachable from the restricted sandbox at `127.77.0.1:<port>`. That covers agentgw's own default `:8080`, sshd and databases.
- This contradicts the Design claim that "loopback services other than the proxy are unreachable".

A second finding: nscd still resolves names for the sandbox, which is a slow DNS exfiltration channel. Owner decisions: a private network namespace, and hiding nscd with a live check.

**Design change (replaces "Template unit, when egress is enforced").**

- **The restricted `agentgw-action@` gets `PrivateNetwork=yes`.**
  - Its namespace has only its own `lo`, so no host address or port is reachable by construction.
  - `IPAddressDeny=any` plus `IPAddressAllow=127.0.0.1/32` stay on as a second layer.
- **The proxy also listens on a unix socket**, `/run/agentgw/egress.sock`, owned by `agentgw:agentgw-io` with mode `0660`. The restricted template bind-mounts it in.
  - The TCP listener on `server.egress.listen` is kept for `sandbox: none` and for the open template's tests. It is unchanged.
- **`exec-job` (already the first process in the unit) starts a forwarder when the job has egress.**
  - The forwarder listens on `127.0.0.1:3128` inside the namespace, and each connection is piped to the unix socket.
  - The run's `HTTPS_PROXY` then points at `http://run-<id>:<token>@127.0.0.1:3128`. Proxy authentication, allowlists and SSRF checks are unchanged, because the proxy still sees every CONNECT.
  - The forwarder stops when the agent process exits.
- **The restricted template also hides `/run/nscd`** (as well as `/run/dbus` and `/run/systemd/resolve`, already done). The sandbox then has no name resolution at all, and the proxy resolves names.
  - **Risk:** user and group lookups for the DynamicUser go through nscd on NixOS. A CLI that calls `getpwuid` (Node's `os.userInfo()`) could fail.
  - **Verification:** the VM test runs the real `claude` and `codex` packages inside the restricted template (`--version`, plus one prompt that must fail only at the network, through the proxy). This is in addition to the stubs.
- **The open template is unchanged.**

**Alternatives rejected.**

- An nftables cgroup rule limiting the action slice to `127.77.0.1` tcp/3128. It needs a slice that always exists and the nftables backend, and it is more fragile.
- Documenting the gap and following up later: rejected by the owner.

**Verification added.**

- In the VM test, agentgw listens on `0.0.0.0:8080`. The probe must fail to reach both `127.77.0.1:8080` and `127.0.0.1:8080`.
- The probe also checks that `getent hosts external` fails.
- An allowed host still works through the proxy.
