---
status: draft
issue: 72
spec: spec/2026-10-13-72-bridge-proxy-env.md
---

# Plan: bridged MCP servers reach their API through the egress proxy

## Approved decisions

These are carried over from the spec.

- **The fix:** `mcpbridge.Run` passes `HTTPS_PROXY`, `HTTP_PROXY`,
  `https_proxy`, `http_proxy`, `NO_PROXY` and `no_proxy` from its own
  environment to the server, alongside `PATH`, `HOME` and `LANG`. Only this
  explicit list; not the whole unit environment.
- **The proxy URL's per-job token** may reach the server, and stays masked.
  `egressSetup`'s `extra` already lands in `o.Secrets`, and its prefix is
  `run-` (`internal/egress/proxy.go:154`).
- **No other change:** sources without egress get no proxy variables, as
  before.
- **Prove the bug first:** the VM change lands before the fix and fails on
  `main`.
- **Rejected:** the whole `JobSpec.Env`, `PrivateNetwork=false`, and
  per-package wrappers.

**A refinement of the spec, not a change of design.** The spec names a new
`TestBridgePassesProxyEnv`. The existing `TestBridgeRelaysToolsOverSocket`
already starts the bridge and asserts that an unrelated variable (`OUTER`)
doesn't leak. So the proxy assertions go into that test, instead of copying
its ~50 lines of setup.

## Steps

1. **VM test first: `nix/stub-mcp-stdio.py` and `nix/vm-test.nix`.**
   - **The stub, lines 10-25 and 41-43:** compute
     `proxy-env=set|missing` from `os.environ.get("http_proxy")`. On
     `tools/call`, do two `urllib.request.urlopen` calls with a 10 s
     timeout. Python's `urllib` reads `http_proxy` itself.
     - `http://external:8080/` → `fetch=<body stripped>`, or
       `fetch=error` on any exception.
     - `http://external:8000/` → `fetch-blocked=ok` if it answers 2xx,
       otherwise `fetch-blocked=refused`.
     - Append `proxy-env=… fetch=… fetch-blocked=…` to the `whoami` text.
       Never print a variable's value.
     - Only do the fetches when `STUB_FETCH=1`, so the AWS case (same
       stub) isn't slowed or changed.
   - **`vm-test.nix:94-98`, `mcpPackages.stub`:** add
     `hosts = [ "external:8080" ];`.
   - **`STUB_FETCH`:** check how `env` flows. `mcpPackages.stub.env` lists
     the allowed names (`STUB_TOKEN`), and the source sets values
     (`sources.stubsrc.env`, `vm-test.nix:199-203`).
     - Add `STUB_FETCH` to the package's `env` list.
     - Set `env.STUB_FETCH = "1"` on `stubsrc`. A literal is fine: it isn't
       a secret, but check that validation accepts a literal env value.
       If not, use a `file:` under `/etc/siphon` like `stub-token`.
   - **The "MCP bridge" subtest (`vm-test.nix:753-770`):** after the
     existing asserts, add:
     - `assert "proxy-env=set" in out`;
     - `assert "fetch=external-ok" in out`;
     - `assert "fetch-blocked=refused" in out`;
     - `assert "run-" not in out`.
   - **Verify:** `nix build .#checks.x86_64-linux.vm -L` **fails** at the
     MCP bridge subtest with `proxy-env=missing`. Save the failing line for
     the PR.
   - **Traps:**
     - Nix merges: edit the attrset in place, no `//` on nested settings.
     - `pkill -f` isn't used here.
     - A slow VM: 10 s timeouts per fetch, inside the subtest's existing
       120 s wait.

2. **The fix: `internal/mcpbridge/bridge.go:44`.** Extend the list
   `[]string{"PATH", "HOME", "LANG"}` with the six proxy names. One line,
   plus a one-line comment on why (the unit has no network except the
   proxy).
   - **Verify:** `go test -race ./internal/mcpbridge`.
   - **Traps:** none.

3. **The unit test: `internal/mcpbridge/bridge_test.go:43-110`,
   `TestBridgeRelaysToolsOverSocket`.**
   - Add `t.Setenv("HTTPS_PROXY", "http://run-x:tok@127.0.0.1:1")` and
     `t.Setenv("NO_PROXY", "")` next to the existing `OUTER`.
   - After the `OUTER` check, assert that `call("HTTPS_PROXY")` equals
     `"HTTPS_PROXY=http://run-x:tok@127.0.0.1:1"`.
   - **Verify:** with step 2 reverted the test fails; with it, the test
     passes (`go test -race ./internal/mcpbridge -run TestBridgeRelaysToolsOverSocket`).
   - **Traps:** the socket path must be ≤ 107 bytes. The test already uses
     `t.TempDir()` and passes in CI, so leave that as it is.

4. **All checks.**
   - Run `go vet ./... && go test -race ./...`.
   - Run `nix build .#checks.x86_64-linux.vm -L`: it passes, with the three
     new asserts.
   - **Verify:** both pass.
   - **Traps:** none.

## Tests

```sh
go test -race ./internal/mcpbridge        # passes after step 2 (fails on HTTPS_PROXY before)
go vet ./... && go test -race ./...
nix build .#checks.x86_64-linux.vm -L     # fails after step 1 alone; passes after step 2
```

## Rollback

Revert the PR: the one-line list, the test assertions and the VM stub
changes. There's no data, config or schema change.

## Implementation

There are 4 files: `bridge.go`, `bridge_test.go`, `vm-test.nix` and
`stub-mcp-stdio.py`. That meets the model split's "three or more files", so
a `coder` agent writes steps 1-3 and a fresh Opus agent reviews them. Step 4
and the VM runs are mine.
