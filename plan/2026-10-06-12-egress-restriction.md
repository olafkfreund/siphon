---
status: approved
issue: 12
spec: spec/2026-10-06-12-egress-restriction.md
---

# Plan: Built-in egress restriction for sandboxed agents and actions

## Approved decisions (self-contained)

- **Proxy.** agentgw serves a stdlib **HTTP CONNECT proxy**, listening by default on `127.77.0.1:3128` (`server.egress.listen`, which must be a loopback address).
  - Only `CONNECT` is accepted. Anything else gets 405.
  - The per-run credential is `Proxy-Authorization: Basic run-<id>:<token>`, with a 32-byte random token that expires when the run ends. Missing, bad or expired credentials get 407.
  - Allowed: `host:443`, or the exact `host:port` of an allowed MCP URL. A host is allowed on an exact name match, or by a `*.suffix` pattern matching subdomains only. Anything else gets 403.
  - The proxy resolves the host and applies the existing SSRF guard (private, link-local and metadata addresses refused unless that entry came from an `allow_private` MCP source), then tunnels bytes with no TLS interception.
  - It counts blocked `host:port` per run.
- **Sandbox (NixOS module).** `services.agentgw.egress.enable` (default `true`).
  - **Restricted template** `agentgw-action@`: `IPAddressDeny=any`, `IPAddressAllow=<proxy ip>/32`. This replaces the metadata-only deny when egress is on.
  - **Open template** `agentgw-action-open@`, identical apart from network: it keeps the metadata deny only.
  - polkit allows start/stop/reset-failed for both `^agentgw-action(-open)?@[0-9a-f]{16}\.service$`.
- **Per-run env** (never argv): `HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy`, `http_proxy` = `http://run-<id>:<token>@<listen>`, with `NO_PROXY` and `no_proxy` empty. Agent runs also get `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`.
- **Effective allowlist** = built-in provider hosts for (kind, auth mode), plus the `host:port` of each agent MCP source with a `url`, plus `agents.<n>.egress.allow`, plus `server.egress.allow`.
  - claude subscription: `api.anthropic.com`, `platform.claude.com`; claude api_key: `api.anthropic.com`.
  - codex subscription: `chatgpt.com`, `auth.openai.com`; codex api_key: `api.openai.com`.
  - agy subscription: `oauth2.googleapis.com`, `daily-cloudcode-pa.googleapis.com`, `cloudcode-pa.googleapis.com`, `www.googleapis.com`, `lh3.googleusercontent.com`; agy api_key: `generativelanguage.googleapis.com`.
- **Defaults.** Agents: `egress.enabled` defaults to true. `cmd` actions are off unless `rules[].egress.enabled` or `server.egress.cmd_default: true`; such a cmd gets only its listed hosts. Routine steps inherit by step kind.
  - A run with egress off uses the open template and gets no proxy env.
  - `sandbox: none`: env is still set, and `validate` warns that it isn't enforced.
- **Visibility.**
  - `validate -v` prints each agent's and egress-enabled rule's effective allowlist.
  - The job output gets a trailing `egress: blocked <host:port> (N)` line.
  - Audit `egress_blocked`, at most one row per host per run.

## Steps

Each step is one commit; cite "Plan step N". Run `go test -race ./...` and `devenv test` after each.

| # | Step | Who | Lane |
|---|---|---|---|
| 1 | Egress proxy package + exported SSRF host check | Codex | new `internal/egress`; `internal/source/netguard.go` (export a host/IP check only) |
| 2 | Config: `server.egress`, `agents.*.egress`, `rules[].egress`, the allowlist builder with the built-in provider table, `validate` output and warnings, schema, README | coder | `internal/config`, `cmd/agentgw` (validate -v only), README, schema |
| 3 | Action: proxy env injection and the per-run template choice (restricted vs open) | Codex (after 1) | `internal/action` |
| 4 | Integration: start the proxy in serve/run-once, register/unregister the per-run allowlist, blocked summary + audit | Opus | `internal/job`, `cmd/agentgw` (serve wiring) |
| 5 | NixOS: `egress.enable`, two templates, IP filter, polkit pattern; VM test with an "external" test host | Opus | `nix/` |
| 6 | Live checks (claude, codex; agy after quota) + PR + CI | Opus | none |

1. **`internal/egress`.**
   - `type Proxy`: `New(listen string, guard func(host string, allowPrivate bool) ([]netip.Addr, error))`, `Start(ctx)`, `Register(allow []Entry) (proxyURL string, blocked func() map[string]int, release func())`.
   - `Entry{Host string; Port int (0 = 443); AllowPrivate bool}`.
   - Constant-time token comparison. Bounded concurrent tunnels (a semaphore of 256). Idle and handshake timeouts. Header read capped at 8 KiB.
   - Tests: allowed/403/407/expired/405, `*.` matching, the port rule, the private-IP refusal, a blocked counter, and an end-to-end tunnel to an `httptest` TLS server.
   - **Trap:** never log the token. The SSRF check must run on the resolved IPs, and the dial goes to the checked IP (no re-resolve).
2. **Config.**
   - Structs and defaults as above.
   - `(*Config).EgressAllow(agent string) []egress.Entry`. Config may import egress for the `Entry` type, or define its own `HostPort` type that the integrator maps.
   - `RuleEgressAllow(rule)`.
   - `validate`: the listen address must be loopback. Warn on `sandbox: none`.
   - `validate -v` prints the lists. Regenerate the schema.
   - README: the egress section with the host table and dates, how to add hosts, the residual risk, and the template note.
   - Tests: list assembly per kind and mode, MCP host:port extraction, defaults, the loopback error, the warning.
3. **Action.**
   - `SandboxOptions` gains `Egress *EgressEnv{ProxyURL string}` (nil = open).
   - In systemd mode, the unit name is `agentgw-action@<id>` when Egress is set and `agentgw-action-open@<id>` when it isn't. `StopOrphans` covers both patterns.
   - Env injection as decided, via `JobSpec.Env` (never argv). Claude gets the non-essential-traffic variable.
   - Tests: the env contains the proxy and not argv; template selection; StopOrphans patterns.
4. **Integration.**
   - `serve`/`run-once` start `egress.Proxy` when any run may need it.
   - Per run: build the allowlist (agent, or a rule's cmd), Register, pass the URL in the sandbox options, Release after the run, then append the blocked summary to the output and write audit rows.
   - Tests with `sandbox: none` and the real proxy: a stub agent using curl through `HTTPS_PROXY` reaches an allowed `httptest` host and gets 403 for another; the blocked line and audit appear.
5. **NixOS.**
   - Module options and templates as decided. The polkit regex covers both templates.
   - VM test: a second node `external` serving HTTPS (self-signed; the stub uses `curl -k`).
   - Subtests:
     - an agent stub reaches `external` via the proxy when it's in `egress.allow`
     - a disallowed host gets 403 and is audited
     - a raw-IP connect to `external`, bypassing the proxy, fails
     - agentgw's API at 127.0.0.1:8080 is unreachable from the sandbox
     - a cmd without egress reaches `external` directly
   - All existing subtests stay green.
   - **Trap:** the IP filter must allow exactly the proxy IP; test from inside the unit.
6. **Live and PR.**
   - With the real proxy and built-in lists only: one codex subscription run, one claude run (subscription if the token is fresh, else API key with a dummy expecting 401 through the proxy), and agy after its quota resets.
   - Record the results; open the PR; CI must be green.

## Tests

`go test -race ./...`, `devenv test`, `nix build .#checks.x86_64-linux.vm` (all subtests), and PR CI green.

## Handoff and review

- **Who:** Codex does steps 1 and 3 (in the `../MCP-AgentGateway-codex` worktree on branch `feat/12-codex-lane`); the coder does step 2; Opus does 4–6.
- **Order:** steps 1 and 2 run in parallel; 3 follows 1; 4 follows 2 and 3; 5 follows 4.
- **Review:** this is a security boundary, so a fresh Opus reviewer gets this plan plus the full diff before the PR. It focuses on proxy auth, the SSRF/rebinding path, IP-filter correctness, and token leakage.

## Rollback

Revert the merge. Operators can disable it without a revert: `services.agentgw.egress.enable = false`, or per agent `egress.enabled: false`.

## Deviations log
- **Step 4:** the proxy also starts lazily on a run's first use (`egressFor`), so pipelines used directly (tests, `rules test`) fail closed instead of erroring for lack of a `serve` start. Tests listen on an ephemeral port via a package-level override (`egressListenOverride`, set in `TestMain`). `AgentEgress` reads the live config, not the per-job agent snapshot: the allowlist follows a reload, which is the stricter choice if hosts were removed. The step 4 test uses a Go helper process as the stub agent instead of curl (no curl dependency in `go test`).
- **Step 5:** the VM test's `external` node serves plain HTTP on 8080 (tunnelled with `curl --proxytunnel`) instead of self-signed HTTPS: CONNECT carries bytes either way, and it drops the cert setup. `external` reaches the allowlist as a LAN MCP source with `allow_private`, the path a real deployment uses for a private host. The VM test found a proxy bug fixed in this commit in `internal/egress` (step 1's lane): the proxy dialled only the first checked address, so a dual-stack name listing an unreachable IPv6 address first failed. It now tries each checked address in order (still no re-resolve); `TestDialFallback` covers it. The existing orphan checks now match both templates, since a plain cmd runs in `agentgw-action-open@`.
- **Step 6, live (2026-10-06):** run with `run-once`, `sandbox: none` and the real proxy, using only the built-in lists.
  - **codex (subscription):** "OK". It was refused `ab.chatgpt.com` and five `*.oaiusercontent.com` hosts, and the run was unaffected. This matches the spec's "seen but not needed".
  - **claude (subscription, token 7.6 h from expiry, so no refresh):** "OK". It was refused one host, `matrix.freundcloud.org.uk`, which comes from the operator machine's own Claude setup (not the CLI) and is harmless.
  - No proxy credential appears anywhere in the state DB.
  - **agy:** deferred until its quota resets; its host set was verified in the spec evidence.
- **Security review fixes (fresh Opus reviewer):**
  - `AgentEgress` now takes the agent definition that actually runs (the job snapshot) and fails closed. A nil agent, or a snapshot without an egress field, is restricted. This reverses the step 4 "live config" choice, which could send a renamed agent's queued job to the open template.
  - A cmd whose rule has gone away still honours `server.egress.cmd_default`.
  - Blocked hosts are capped at 64 distinct names per run (the rest count as `other`) and are made printable and at most 255 bytes, which bounds the audit rows and output lines.
  - The proxy's name resolution has a 5 s timeout.
  - `server.egress.listen` must be IPv4, because the module derives `IPAddressAllow=<ip>/32` from it.
  - `validate` also warns for egress-enabled rules and `cmd_default` under `sandbox: none`.
  - The restricted template hides `/run/dbus` and `/run/systemd/resolve`, closing DNS exfiltration over local sockets; the VM probe asserts the bus is gone.
  - The README notes that with `egress.enable = false` the proxy variables are not enforced.
  - **Open, needs an owner decision:** the IP filter is IP-only, so services bound to all addresses are reachable on 127.77.0.1:<port>. nscd also still resolves names for the sandbox.

## Amendment 1 (status: draft): private netns, unix-socket proxy, no nscd

This implements spec Amendment 1 (approved). These decisions carry over and are binding:
- The restricted template gets `PrivateNetwork=yes`, keeps `IPAddressDeny=any`, and changes `IPAddressAllow` to `127.0.0.1/32`.
- The proxy also listens on a unix socket at `/run/agentgw/egress.sock` (`agentgw:agentgw-io`, `0660`), which is bind-mounted into the restricted template.
- `exec-job` forwards `127.0.0.1:3128` inside the namespace to that socket. The run's `HTTPS_PROXY` points there, and proxy auth and allowlists are unchanged.
- The TCP listener stays, for `sandbox: none`.
- The restricted template also hides `/run/nscd`.
- The open template is unchanged.

| # | Step | Who | Lane |
|---|---|---|---|
| 7 | Proxy unix listener + config | coder | `internal/egress`, `internal/config`, `internal/job` |
| 8 | Forwarder in exec-job + env rewrite | coder (after 7) | `internal/action` |
| 9 | Module: netns, bind, nscd, socket setting | Opus | `nix/module.nix` |
| 10 | VM test: wildcard-bound API, no DNS, real CLIs | Opus | `nix/vm-test.nix` |

7. **Unix listener.**
   - `server.egress.socket` is a string, default `""` (no unix listener); the module sets it. `validate` requires an absolute path when it is set.
   - `(*Proxy).ServeUnix(ctx, path)`:
     - removes a stale socket, listens, `chmod 0660`, and changes the socket's group to the parent directory's group (agentgw is a member);
     - serves with the same handler as TCP and removes the socket when ctx is done.
   - `startEgress` starts it next to TCP when it is set, and a start error stops agentgw, the same as TCP.
   - Test: CONNECT through the unix socket reaches an allowed `httptest` host and gets 403 for another.
8. **Forwarder.**
   - `EgressEnv` gains `Socket string`. In systemd mode, when `Socket != ""`:
     - the proxy URL's host part is rewritten to `127.0.0.1:3128` (the credentials are kept);
     - `JobSpec` gains `EgressSocket string`.
   - In `execJob`, when `spec.EgressSocket != ""`:
     - listen on `127.0.0.1:3128` before starting the child;
     - for each connection, dial the unix socket and copy both ways;
     - close the listener when the child exits.
     - If the listen fails, the job fails (exit 1, with a message on stderr). It never runs without the forwarder.
   - `sandbox: none` and an empty socket are unchanged (TCP URL).
   - Masking still covers the token.
   - Tests: the env rewrite and the job spec field via the fake systemd seam; an `execJob` unit test with a temp unix socket echo server, where the child (a test helper) connects to `127.0.0.1:3128` and gets the echo. Skip if the port is taken.
9. **Module.**
   - agentgw.service: `RuntimeDirectory = "agentgw"`, `RuntimeDirectoryMode = "0750"`, with the group set via `RuntimeDirectory` ownership to `agentgw-io` (or chgrp in the Go step; whichever works under `RestrictSUIDSGID`, logged as a deviation).
   - Set `settings.server.egress.socket` by default to `/run/agentgw/egress.sock` when `egress.enable`.
   - Restricted template:
     - `PrivateNetwork = true`
     - `IPAddressAllow = [ "127.0.0.1/32" ]`
     - `BindPaths = [ "/run/agentgw/egress.sock" ]`
     - add `-/run/nscd` to `InaccessiblePaths`.
   - The open template is unchanged.
10. **VM test.**
    - agentgw listens on `0.0.0.0:8080`; the existing curls still use 127.0.0.1.
    - The probe must fail to reach `127.77.0.1:8080`, `127.0.0.1:8080` and the external IP (taken from `/etc/hosts`), and `getent hosts external` must fail.
    - The allowed host still works through the proxy.
    - The real `claude-code` (unfree, allowed in the test's pkgs) and `codex` packages are added to `agentPackages` and run inside the restricted template through a cmd with egress enabled. Each runs `--version` (exit 0), then one prompt with a dummy API key that must fail with a network or auth error, not a user-lookup crash.
    - All existing subtests stay green.

**Order:** 7 → 8 → 9 → 10.

**Review:** the same fresh Opus security reviewer, on the amendment's diff, before the PR.

**Rollback:** revert the amendment commits. The previous design (IP filter only) still works.
