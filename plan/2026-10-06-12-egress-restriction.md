---
status: draft
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
