---
status: approved
issue: 72
intent: intent/2026-10-13-72-bridge-proxy-env.md
---

# Spec: bridged MCP servers reach their API through the egress proxy

## Design

**The fix is one list.** `mcpbridge.Run` (`internal/mcpbridge/bridge.go:44`)
copies a fixed list of variables from its own environment into the
server's: `PATH`, `HOME`, `LANG`. Add the six proxy variables that
`egressSetup` (`internal/action/cmd.go:225-228`) sets on the bridge unit:
`HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy`, `http_proxy`, `NO_PROXY` and
`no_proxy`. As approved in the intent, only these are passed: an explicit
list, not the whole unit environment.

When a source has no egress, `bridgeUnit` sets no proxy variables
(`bridge.go:228`), so nothing changes there.

**The credential.** The proxy URL carries a per-job token. It already
reaches the bridge unit, and `egressSetup` returns it in `extra`, which goes
into `o.Secrets` and is masked in job output and errors
(`bridge.go:231`, `bridgeMask`). The server now sees it too, which is how
it authenticates to the proxy. The proxy only lets it reach
`BridgeEgress(src)`: the package's `hosts` plus its `url_env` hosts. A
leaked token can't widen that, and it dies with the job.

**Unit test.** `TestBridgePassesProxyEnv`, in
`internal/mcpbridge/bridge_test.go`. It reuses the `TestStub` helper, whose
`env` tool reports a variable by name, and the setup
`TestBridgeRelaysToolsOverSocket` already uses.
- It sets `HTTPS_PROXY`, `NO_PROXY` and an unrelated `SIPHON_TEST_OTHER` in
  the test process (`t.Setenv`), then runs `Run`.
- The `env` tool must report both proxy values, and an empty
  `SIPHON_TEST_OTHER`, so the list stays explicit.

**VM test** (`nix/vm-test.nix`, `nix/stub-mcp-stdio.py`), in the existing
"MCP bridge" subtest:
- `mcpPackages.stub` gets `hosts = [ "external:8080" ]`.
- The stub's `whoami` also does two GETs through the proxy, using Python's
  `urllib` (which reads `http_proxy`), with a 10 s timeout each:
  - `http://external:8080/`, reported as `fetch=<body>`;
  - `http://external:8000/`, which isn't in the package's hosts, reported
    as `fetch-blocked=<refused|ok>`.
- The stub never prints a variable's value. It reports
  `proxy-env=set|missing` only.
- Assertions: `proxy-env=set`, `fetch=external-ok` and
  `fetch-blocked=refused`, and no `run-` (the proxy token's prefix) in the
  job output. The existing secret assertions stay.

**Proving the bug first:** the VM change goes in before the fix. On `main`
it must fail with `proxy-env=missing` and no `fetch=external-ok`.

## Alternatives rejected

- **Pass the whole `JobSpec.Env` through.** That's broader than needed. A
  later variable added to the unit would leak into third-party servers
  without anyone deciding it should. The intent chose the explicit list.
- **Give `siphon-mcp@` real network (`PrivateNetwork=false`) with an
  IP allowlist.** That loses host-based filtering and the audit of blocked
  hosts, which the proxy gives.
- **Fix it per package** (e.g. a wrapper that exports the proxy). Every
  package would need one, and the bridge is the one place that clears the
  environment.

## Risks

- **Existing bridged packages start reaching the network.** That's the
  intended behaviour, but a package that relied on failing fast could now
  make real calls. Its hosts are still limited to what the operator listed.
- **A server that ignores proxy variables** still can't reach its API.
  Python `urllib`/`requests`, Node `undici` (with `NODE_USE_ENV_PROXY`) and
  Go's `http.ProxyFromEnvironment` read them. Node's default `fetch` doesn't
  before Node 24. Packages like that are out of scope here, and the
  ServiceNow server (#74) is Go.
- **`NO_PROXY` is empty,** as set by `egressSetup`, so even loopback goes
  through the proxy. That's the same as the agent sandbox.

## Verification

- `go test -race ./internal/mcpbridge -run TestBridgePassesProxyEnv` fails
  before the fix (the proxy values are empty) and passes after it.
- `nix build .#checks.x86_64-linux.vm`:
  - with only the VM changes, it fails at the MCP bridge subtest
    (`proxy-env=missing`);
  - with the fix, it passes, with `fetch=external-ok` and
    `fetch-blocked=refused`.
- `go vet ./... && go test -race ./...` pass.
