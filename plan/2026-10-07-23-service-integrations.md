---
status: approved
issue: 23
spec: spec/2026-10-07-23-service-integrations.md
---

# Plan: Service integrations: building blocks, GitHub and GitLab

AWS is #24 and is out of scope.

## Approved decisions (self-contained)

- **Webhook modes.**
  - **`signature: token`:** `token_header` is required. The header is
    compared with the secret in constant time on SHA-256 digests. A miss
    gets 401. `validate` warns that it's weaker than an HMAC.
  - **`signature: standard-webhooks`:**
    - headers `webhook-id`, `webhook-timestamp` and `webhook-signature`
      (space-separated `v1,<base64>`);
    - the signed content is `id.timestamp.body`, HMAC-SHA256 with the
      decoded secret (`whsec_` means base64), within ±5 min;
    - `webhook-id` is the default replay key.
  - Both run before parsing, with the existing replay store and body cap.
- **Stdio MCP `env`.**
  - `type: mcp` sources with `command`/`package` get
    `env: {NAME: <Secret ref>}`.
  - Names must match `^[A-Z_][A-Z0-9_]*$`.
  - Denylist, even in the file: `LD_*`, `DYLD_*`, `PATH`, `HOME`,
    `NODE_OPTIONS`, `PYTHON*`, `BASH_ENV`, `ENV`, `PERL5*`, `RUBY*`,
    `JAVA_TOOL_OPTIONS`, `SSL_CERT_*`, `GIT_*`, `*_PROXY`.
  - **Overlay:** keys are file-only. The portal may change the values of
    existing keys (F2 ref rules). Package sources take their keys from the
    package.
  - **Daemon polling:** the env goes only into that child's minimal env.
- **The MCP bridge,** used for agent runs when a stdio server has `env`.
  - A new restricted template, `siphon-mcp@<16hex>`: its own DynamicUser,
    the same hardening as `siphon-action@` (`PrivateNetwork`, egress forwarder,
    `ProtectProc=invisible`), and a lifetime bound to the run.
  - It runs `siphon mcp-bridge <spec>`: start the server with its env, and
    serve it as streamable HTTP on a unix socket in the run's bridge dir.
  - The agent unit's `exec-job` forwards `127.0.0.1:<port>` to that
    socket, and the agent's MCP config points at
    `http://127.0.0.1:<port>/mcp`.
  - Without `env`, today's behaviour is unchanged (the server starts inside
    the agent unit).
  - The polkit regex gains `siphon-mcp@`.
- **Packages.**
  - `services.siphon.mcpPackages` (default `{ github = pkgs.github-mcp-server; }`)
    renders to file-only
    `server.mcp_packages.<name> = {command, env (key names), hosts}`.
  - A source with `package: <name>` takes its command, env keys and default
    egress hosts from that entry.
  - The portal can use only listed packages.
- **The Services page** (`/services`): GitHub and GitLab tiles that create
  ordinary items.
  - **GitHub:**
    - remote MCP (`https://api.githubcopilot.com/mcp/` with `auth.bearer`)
      or `package: github` with env `GITHUB_PERSONAL_ACCESS_TOKEN`;
    - an optional webhook (`signature: github`, a generated 32-byte secret
      shown once, the URL and steps);
    - read-only or read-write tool allowlist templates;
    - **Test:** MCP lists tools; `GET /user`.
  - **GitLab:**
    - a base URL (gitlab.com or self-hosted) and a token;
    - a REST `http` source with a `PRIVATE-TOKEN` header ref;
    - an optional webhook (`signature: token` on `X-Gitlab-Token`, or
      `standard-webhooks` if the instance supports it), with a generated
      secret shown once;
    - an MCP endpoint if one is probed;
    - **Test:** `GET /api/v4/user`.
  - New file-only settings: `server.public_url` (used to show webhook URLs)
    and `server.services.private_endpoints` (LAN or self-hosted endpoints,
    the same rule as models).
  - Tokens are write-only and never echoed.
- **All earlier guarantees hold:** F1 (command file-only), F2 (ref
  confinement), egress, masking, CSRF/auth, and the CSP template test.

## Steps

Each step is one commit; cite "Plan step N". Run `go vet ./...`,
`go test -race ./...` and `devenv test` after each, and the VM test after
steps 4 and 6.

| # | Step | Who | Lane |
|---|---|---|---|
| 1 | Webhook modes `token` and `standard-webhooks` (+ config, schema, `validate` warning) | coder | `internal/source/webhook.go`, `internal/config` |
| 2 | Stdio MCP `env` (config, denylist, overlay rules, daemon-side injection, masking) | coder | `internal/config`, `internal/source/mcp.go`, `internal/job` |
| 3 | MCP packages: `server.mcp_packages`, `package:` resolution, overlay rules (Go); `services.siphon.mcpPackages` (Nix) | coder (Go), Opus (Nix) | `internal/config`, `internal/job`, `nix/module.nix` |
| 4 | MCP bridge: `siphon mcp-bridge`, unit start and stop per run, `exec-job` port forwards, agent MCP config rewrite (claude, codex, agentloop) (Go); `siphon-mcp@` template + polkit + VM subtest (Nix) | coder (Go), Opus (Nix) | `cmd/siphon`, `internal/action`, `internal/agentloop`, `internal/job`, `nix/` |
| 5 | Services page: GitHub and GitLab presets, generated webhook secrets, Test, `public_url`, `services.private_endpoints` | Opus | `internal/web`, `internal/config` (settings only) |
| 6 | VM subtests for the webhook modes, README sections, security review, live checks (owner's GitHub repo and GitLab project), PR | Opus | `nix/vm-test.nix`, README |

1. **Webhook modes** (coder).
   - `Source` gains `TokenHeader`. `Signature` accepts `token` and
     `standard-webhooks`.
   - Validation: `token` needs `token_header`; `standard-webhooks` takes no
     `signature_header` or `timestamp_header`. Warn on `token`.
   - **Verify functions:**
     - token: `subtle.ConstantTimeCompare(sha256(header), sha256(secret))`;
     - standard-webhooks: parse the three headers, decode the `whsec_` key,
       check the timestamp skew, compute the HMAC, and compare in constant
       time against each `v1,` entry.
   - The replay key defaults to `webhook-id`.
   - **Tests:** valid; wrong, missing or malformed; skew both ways;
     multiple signatures, one valid; a replay is refused; the GitLab header
     name; a `standard-webhooks` vector from the spec site, recomputed in
     the test.
2. **Stdio env** (coder).
   - `Source.Env map[string]Secret`, resolved like the other secrets and
     masked through `Secrets()`.
   - Validation: the name pattern and the denylist; `env` only with
     `command`/`package`.
   - **Overlay (`checkOverlay`):** a portal item may not add or remove env
     keys relative to the file's item (or the package's key list). Values
     follow F2.
   - **Daemon poll:** `source.MCPOptions.Env` adds the values to the child's
     env, next to the existing allowlisted base. Never in argv; error
     strings scrubbed.
   - **Agent runs, temporarily until step 4:** a stdio source with `env`
     used by an agent fails the run with "needs the MCP bridge (coming in
     step 4)". It's never passed in plain form.
   - **Tests:** validation table; overlay add, remove and rotate; the poll
     child sees the env (a stub server prints its env to a temp file);
     masking; the agent run refuses.
3. **Packages** (coder for Go, Opus for Nix).
   - Go:
     - `Server.MCPPackages map[string]MCPPackage{Command []string; Env []string; Hosts []string}`,
       file-only through `sameFixedSections`;
     - `Source.Package string`, mutually exclusive with `Command`/`URL`;
     - resolution fills in the command and the allowed env keys;
     - `AgentEgress` adds the package hosts for agents using the source;
     - the overlay may set `package:` to listed names only.
   - Nix: `services.siphon.mcpPackages` (attrsOf package plus metadata;
     defaults for github: env `GITHUB_PERSONAL_ACCESS_TOKEN`, hosts
     `api.github.com`), rendered into settings.
   - **Tests:** resolution; an unlisted package is refused; the portal can't
     set `command` alongside `package`; the egress hosts; module eval.
4. **MCP bridge** (coder for Go, Opus for Nix).
   - `siphon mcp-bridge <spec.json>`:
     - starts the server (command plus env from the spec, with secret
       values read from files in its private `/tmp`);
     - relays streamable HTTP ↔ stdio with the go-sdk, serving on
       `<bridge dir>/mcp.sock`;
     - exits when stdin closes or on SIGTERM.
   - **The runner,** for each stdio server with `env` used by an agent run:
     - write the bridge `job.json`, start `siphon-mcp@<id>` (polkit allows
       it), and wait for the socket (10 s);
     - add a `Forwards: {port: socketPath}` entry to the agent's
       `JobSpec`. `exec-job` serves those, as it does egress;
     - point the agent's MCP config at `http://127.0.0.1:<port>/mcp`;
     - stop the bridge when the agent run ends, and on `StopOrphans`.
   - **Directories:** a per-run bridge dir under the actions dir, setgid
     `siphon-io`, with the socket mode 0660.
   - Nix (Opus):
     - the `siphon-mcp@` template (from `actionUnit` with the restricted
       network: its own egress allowlist from the spec, through the
       forwarder);
     - `ReadWritePaths` for the bridge dir;
     - the polkit regex `^siphon-(action(-open)?|mcp)@[0-9a-f]{16}\.service$`;
     - VM subtest: a `kind: model` agent uses a stdio stub MCP server with
       `env: {STUB_TOKEN: file:…}`. The tool returns a hash of the token
       (proving the server got it). Inside the agent unit,
       `grep -r <token> /proc/*/environ /tmp` finds nothing, and the bridge
       runs as a different uid.
   - **Tests (Go):** the bridge relays `tools/list` and `tools/call`; the
     runner rewrites the MCP config for all three agent kinds; forwards;
     cleanup.
5. **Services page** (Opus).
   - `/services` with GitHub and GitLab tiles.
   - Forms create items through `commit`, so all validation and secret
     handling apply.
   - **Generated webhook secrets:** 32 random bytes (base64 for
     `standard-webhooks`, hex otherwise). They're stored write-only and
     shown **once** on the result page, with the URL
     (`server.public_url` + `/hook/<name>`, or the path plus a reverse-proxy
     note) and the provider's UI steps.
   - **Tool templates:** the page shows the read-only or read-write
     allowlist to paste, with a "copy" button and an "apply to agent…"
     picker.
   - **Test endpoints** use the daemon guard plus
     `server.services.private_endpoints`, and show the latency, the username
     or tool count, and classified errors. They never echo a token.
   - The nav gains a **Services** entry with an icon.
   - **Tests:** the presets create the expected items; secrets are shown
     once and never again; Test output carries no token; an unlisted
     private GitLab is refused; CSP.
6. **VM, docs, review, live, PR** (Opus).
   - VM subtests: a `token` webhook (`X-Gitlab-Token`) and a
     `standard-webhooks` webhook each fire a rule; a wrong token gets 401.
   - README: "GitHub", "GitLab" and "Webhook authentication".
   - A fresh Opus security review: the webhook verify paths, env and
     bridge isolation, package allowlist bypasses, Services secrets, SSRF
     via Test.
   - **Live (owner):**
     - GitHub: a fine-grained token on a test repo; Services → Test; a PR
       webhook via `gh webhook forward` reaches the local instance and fires
       an agent;
     - GitLab: a token and a test project; a merge request webhook with the
       `token` mode fires a rule. It can be tested with `curl` plus the
       header if GitLab can't reach the local instance.
   - Then the PR.

## Tests

`go test -race ./...`, `devenv test`, `nix build .#checks.x86_64-linux.vm`
(all subtests), and the owner's live checks.

## Handoff

- **Coder:** steps 1 → 2 → 3 (Go) → 4 (Go), one agent, each later step sent
  by `SendMessage`.
- **Opus:** the Nix parts of 3 and 4, then 5 and 6. Step 5 can start after
  step 2.

## Rollback

Revert the merge. Existing webhook presets, sources and agents are
unchanged, and only the new fields and the page disappear.

## Deviations log
- **Step 1 (coder; one test added by Opus):**
  - The `token` mode strips its header from the event headers, so rules never see the secret.
  - The replay key stays the body hash for `token`, like the existing presets.
  - The `token` warning fires for every `token` source.
  - Opus added the **published Standard Webhooks reference vector** (`msg_p5jXN8AQM9LWM0D4loKWxJek` / `v1,g0hM9SsE…`) as a test, plus a tampered-signature check. Changing only the final base64 character before `=` is not a forgery: those are padding bits, and the bytes decode the same.
