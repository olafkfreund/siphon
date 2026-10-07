---
status: approved
issue: 23
intent: intent/2026-10-07-23-service-integrations.md
---

# Spec: Service integrations: building blocks, GitHub and GitLab

AWS is #24. This spec covers the shared building blocks plus GitHub and
GitLab.

## Design

### 1. Webhook authentication modes

`type: webhook` sources gain two `signature` values next to `github` and
`sha256`:

```yaml
sources:
  gitlab-hooks:
    type: webhook
    signature: token                 # shared secret in a header
    token_header: X-Gitlab-Token
    secret: file:/run/credentials/siphon.service/gitlab-hook
    id: header.X-Gitlab-Event-UUID
  hooks-std:
    type: webhook
    signature: standard-webhooks     # https://www.standardwebhooks.com
    secret: file:...                 # "whsec_<base64>" or raw bytes
```

- **`token`:**
  - The header value is compared with the secret using
    `subtle.ConstantTimeCompare` on SHA-256 digests (length-independent).
  - `token_header` is required.
  - A missing or wrong header gets 401.
  - Weaker than an HMAC (there's no body integrity), so `validate` warns
    "token auth: prefer standard-webhooks/HMAC where the provider supports
    it".
- **`standard-webhooks`:**
  - Headers `webhook-id`, `webhook-timestamp` and `webhook-signature`.
  - The signed content is `id + "." + timestamp + "." + body`, HMAC-SHA256
    with the decoded secret (a `whsec_` prefix means base64).
  - The signature header is a space-separated list of `v1,<base64>`. Any
    match passes, compared in constant time.
  - The timestamp must be within ±5 min.
  - `webhook-id` is the replay key automatically, unless `id:` is set.
- **Shared behaviour:** both run before any parsing, and the existing
  replay store and body size cap apply.

### 2. Secrets for stdio MCP servers

```yaml
sources:
  gh-local:
    type: mcp
    command: [github-mcp-server, stdio]          # file-only (F1), or a package (3)
    env:
      GITHUB_PERSONAL_ACCESS_TOKEN: file:/run/credentials/siphon.service/gh-token
```

- **`env`** is a map from a name to a `Secret` reference, on `type: mcp`
  sources with `command`/`package`.
  - Names must match `^[A-Z_][A-Z0-9_]*$`.
  - A **denylist** applies even in the file: `LD_*`, `PATH`, `HOME`,
    `NODE_OPTIONS`, `PYTHON*`, `BASH_ENV`, `ENV`, `PERL5*`, `RUBY*`, `JAVA_TOOL_OPTIONS`,
    `DYLD_*`, `SSL_CERT_*`, `GIT_*` and `*_PROXY`.
  - **Portal and overlay:** keys are file-only, like `command` (F1). The
    portal may change only the **values** of keys that already exist in the
    file, within the F2 ref rules, which allows rotating a token.
    Package-based sources (3) take their keys from the package template.
- **When Siphon polls the source** (in the daemon), the values are added to
  that one child process's otherwise minimal environment (today's
  allowlisted base). They're never in argv and are masked in errors.
- **When an agent uses the source,** the server runs through an **MCP
  bridge**, so the agent process never holds the secret:
  - for each stdio MCP server an agent run uses, Siphon starts a separate
    `siphon-mcp@<run>-<n>` instance of a new restricted template: its own
    DynamicUser, the same hardening and egress rules as agents, and its
    allowlist is the source's own `egress.allow` plus the provider hosts
    for packages;
  - that instance runs `siphon mcp-bridge`, which starts the server with
    its env and serves it as streamable HTTP on a unix socket in a per-run
    directory;
  - `exec-job` in the agent's unit forwards `127.0.0.1:<port>` to that
    socket, as the egress forwarder does;
  - the agent's MCP config (Claude, Codex, the built-in loop) points at
    `http://127.0.0.1:<port>/mcp`. The agent's own process never sees the
    env, because a different uid owns it, and `/proc` is invisible to it
    (`ProtectProc=invisible`).
  - **Without secrets** (no `env`), stdio servers keep today's behaviour:
    started inside the agent's unit.
- **The MCP bridge** is a ~150-line stdio↔streamable-HTTP relay using the
  go-sdk transports. Its life is tied to the run: the agent's unit stops it,
  and the template has `BindsTo`.

### 3. Allowlisted MCP server packages (intent Q1 c)

- **NixOS:** `services.siphon.mcpPackages` is an attrset of name → package.
  The default is `{ github = pkgs.github-mcp-server; }` when that package
  exists in nixpkgs. The module renders it into file-only config:

  ```yaml
  server:
    mcp_packages:
      github: { command: ["/nix/store/…/bin/github-mcp-server", "stdio"], env: [GITHUB_PERSONAL_ACCESS_TOKEN], hosts: ["api.github.com"] }
  ```
- **A source may say `package: github`** instead of `command`. Its command,
  env key names and default egress hosts come from `server.mcp_packages`,
  which is file-only.
  - The portal can create and edit `package:` sources and set the values of
    the package's env keys.
  - The portal can't set a `command`, extra env keys, or a package that
    isn't listed.
  - Off NixOS, `server.mcp_packages` is written by hand.

### 4. The Services page and presets

- A new nav item, **Services** (`/services`), with tiles for **GitHub** and
  **GitLab**, built like the model tiles. Saving creates ordinary items
  (sources, credentials, an optional webhook), all editable afterwards, and
  shows a summary.
- **GitHub:**
  - **Inputs:** a name, a fine-grained token (write-only), the mode
    (**remote MCP** at `https://api.githubcopilot.com/mcp/`, or the local
    `package: github`), and optionally a webhook.
  - **What it creates:**
    - an MCP source with `auth.bearer` (remote) or `env`
      (package);
    - if a webhook is wanted, a webhook source with
      `signature: github` and a **generated 32-byte secret**. The secret is
      shown **once**, with the exact URL `https://<your siphon>/hook/<name>`
      and the GitHub UI steps;
    - a **tool allowlist template** to copy into agents: **read-only**
      (`get_*`, `list_*`, `search_*`) or **read-write** (adds review,
      comment, PR and issue tools).
  - **Test:** the MCP server lists tools with the token; GitHub `GET /user`
    (or `/meta`) confirms the token works. Scopes are shown as GitHub
    reports them (the `X-OAuth-Scopes` header for classic tokens; for
    fine-grained ones, "fine-grained (scopes not reported)").
- **GitLab:**
  - **Inputs:** the base URL (default `https://gitlab.com`, any self-hosted
    URL), a personal, project or group access token (write-only), and
    optionally a webhook.
  - **What it creates:**
    - a REST `http` source for polling, for example merge requests or
      pipelines for a project, with a `PRIVATE-TOKEN` header ref;
    - optionally a webhook with `signature: token` and
      `token_header: X-Gitlab-Token`, with a generated secret shown once and
      the GitLab UI steps. If the GitLab instance supports signing tokens,
      the page offers `standard-webhooks` instead.
    - **Agent tools for GitLab:** if the instance offers an MCP endpoint, it
      is added as an MCP source with `auth.bearer`. Otherwise the page says
      agents get GitLab data through rules (event payloads), and a
      Nix-pinned GitLab MCP package can be added later through (3).
  - **Test:** `GET /api/v4/user` with the token (latency, username; no
    token echo), and a probe for the MCP endpoint.
  - **Private or self-hosted GitLab** on a LAN goes through the same
    file-level rule as model endpoints: its `host:port` must be in a
    file-only allowlist, `server.services.private_endpoints`, because both
    the source polling and the Test button run in the daemon.
- **Webhook URL:** the page shows `<server.public_url>/hook/<name>`. There
  is a new optional `server.public_url` setting (file-only). Without it, the
  page shows the path and explains that a reverse proxy is needed for the
  outside world to reach it.

### 5. Docs

README sections: "GitHub", "GitLab", and "Webhook authentication", with
token scopes, the setup steps on each provider, and security notes (least
privilege, read-only by default, rotating tokens on the Connections or
Services page).

## Alternatives rejected

- **Letting the portal set any `command`/`env` for stdio servers:** host
  code execution. It's refused by F1, and the package allowlist (3) is the
  safe alternative.
- **Passing stdio server secrets straight into the agent's unit:** the agent
  process (same uid) could read them through `/proc` or its config files,
  which breaks the intent's constraint. The MCP bridge fixes that.
- **A GitLab-specific HMAC or legacy token check written ad hoc:** the
  generic `token` and `standard-webhooks` modes cover GitLab, EventBridge
  (#24) and many others.
- **Vendored GitHub or GitLab SDKs:** not needed. Plain REST calls are
  enough for Test and polling.

## Risks

| Risk | Mitigation |
|---|---|
| The MCP bridge adds a moving part per agent run | Only for stdio servers with secrets; the same template and forwarder patterns as the egress work; VM-tested; the unit's life is bound to the run |
| `token` webhooks are weaker (no body integrity) | Warning in `validate`; prefer `standard-webhooks` where the provider supports it; TLS in front is assumed (README) |
| A GitHub fine-grained token's scope can't be read back | The page says so plainly; read-only tool templates by default |
| A GitLab MCP endpoint differs by version, or is absent | Probed by Test; fall back to REST plus webhooks; documented |
| `server.public_url` unset makes webhook setup confusing | The page explains the reverse-proxy requirement and shows the path |
| The env denylist misses a dangerous variable | Keys are file-only anyway (the operator writes them), so the denylist is defence in depth; the list is conservative |

## Verification

- **Unit:**
  - both new webhook modes: valid, a wrong secret or signature, a missing
    header, timestamp skew, replay, and constant-time use (code review);
  - env name validation and the denylist;
  - overlay rules: the portal can't add env keys, can rotate values, can use
    a package, and can't use an unlisted package;
  - the bridge relays `tools/list` and `tools/call` between a stdio stub and
    HTTP;
  - Services presets create the expected items;
  - Test endpoints never echo tokens.
- **NixOS VM test:**
  - a `token` webhook and a `standard-webhooks` webhook each trigger a
    rule;
  - a `kind: model` agent uses a stdio MCP server whose env carries a
    secret through the bridge: the tool call works;
  - inside the agent unit, `/proc/*/environ` and the agent's files contain
    no secret, and the bridge unit runs as a different uid.
- **Live (owner):**
  - **GitHub:** create a fine-grained token; Services → GitHub → Test; a
    PR webhook from a test repo triggers an agent that reads the PR
    (through a reverse proxy, or `gh webhook forward` for local testing).
  - **GitLab:** token plus a test project; a merge request event with the
    `token` mode is accepted and fires a rule.
