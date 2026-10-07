---
status: approved
issue: 20
spec: spec/2026-10-07-20-model-providers.md
---

# Plan: Local and remote model providers

## Approved decisions (self-contained)

- **Config.**
  - New `credentials` providers: `ollama` (`url`, no key) and `openai`
    (`url`, optional `api_key` with the existing secret rules, optional
    `preset`, UI only).
  - New agent `kind: model` with a `model:` field. It must use an `ollama`
    or `openai` credential.
  - `url` must be `http(s)://host[:port][/path]`.
  - The YAML key stays `credentials`; the UI says "Connections".
- **The built-in loop.**
  - `siphon agent-run` (a hidden subcommand) runs as the agent command in
    the same `siphon-action@` sandbox.
  - It uses OpenAI-compatible `POST {base}/chat/completions` with `tools`
    built from the agent's MCP servers, filtered by the mandatory
    `allowed_tools` (`mcp__<server>__<tool>`). For Ollama the base is
    `url + "/v1"`.
  - It executes `tool_calls` through MCP, capping each result at 64 KiB,
    for up to `max_turns`.
  - Fallback: a single plain completion when there are no tools or the
    endpoint rejects `tools`.
  - Output: `{"type":"result","result":…,"turns":n,"usage":{…}}`.
  - **The HTTP client tunnels every target (http and https) with CONNECT
    through `HTTPS_PROXY`.** The proxy stays CONNECT-only.
  - The key is read from a file in the run's private `/tmp`, never from env
    or argv, and is masked.
  - The conversation is capped at 512 KiB.
  - `max_budget_usd` applies only when the provider reports a cost.
- **Egress.**
  - A model agent's allowlist includes its connection's `host:port`.
  - Private, loopback or LAN endpoints are allowed only when the exact
    `host:port` is in `server.models.private_endpoints` (file only).
    Portal saves of a connection to an unlisted private endpoint are
    refused, with a message naming the YAML to add.
  - Entries for these endpoints use a new guard mode that allows private
    addresses but **never link-local or metadata** (`169.254.0.0/16`,
    `fd00:ec2::254`).
  - NixOS: `services.siphon.models.privateEndpoints` defaults to
    `[ "127.0.0.1:${toString config.services.ollama.port}" ]` when
    `services.ollama.enable` is on.
- **The Connections page** (renamed from Logins; `/logins` redirects to
  `/connections`).
  - Two sections: Subscriptions (as today) and Models.
  - Tiles: Ollama, LM Studio, OpenRouter, Groq, Mistral and
    OpenAI-compatible. Each pre-fills the URL and shows an original
    monogram badge, not a vendor logo.
  - **Test:** `POST /connections/{name}/test` uses the same guard and
    allowlist, lists models (Ollama `/api/tags`, otherwise `/v1/models`)
    with the latency, returns at most 200 ids, and never response bodies or
    keys.
  - `GET /connections/{name}/models` is cached for 60 s.
  - The agent editor's `kind: model` gets connection and model pickers,
    with a "tools ✓" hint for qwen, llama3.x, mistral, gpt-oss and
    command-r.
  - Runs show the provider badge and the model.

## Steps

Each step is one commit; cite "Plan step N". Run `go vet ./...`,
`go test -race ./...` and `devenv test` after each, and the VM test after
step 5.

| # | Step | Who | Lane |
|---|---|---|---|
| 1 | Config: providers, `kind: model`, validation, `server.models.private_endpoints`, `AgentEgress` for model agents, overlay check (portal private endpoints) | coder | `internal/config` |
| 2 | Guard: a private mode without link-local; `egress.Entry` flag; the job maps entries | coder | `internal/source/netguard.go`, `internal/egress`, `internal/job/egress.go` |
| 3 | Agent loop: `internal/agentloop` (chat client, CONNECT dialer, MCP tools, loop, fallback, output), `siphon agent-run`, the `model` runner in `internal/action` | coder | `internal/agentloop`, `cmd/siphon`, `internal/action` |
| 4 | Web: Connections page, test and models endpoints, agent editor pickers, badges | Opus | `internal/web` |
| 5 | NixOS: the `models.privateEndpoints` option; VM test with a stub model server and a stub HTTP MCP server on `external` | Opus | `nix/` |
| 6 | Live check against the owner's local Ollama (`qwen3.8:27b`), a fresh security review, README "Model connections", PR | Opus | README, none |

1. **Config.**
   - `Credential` gains `URL string` and `Preset string`. `Agent` gains
     `Model string`.
   - `Validate`:
     - the provider is `claude|codex|agy|ollama|openai`;
     - `url` is required and parses for `ollama`/`openai`, and is not
       allowed for the others;
     - `api_key` is not allowed for `ollama`;
     - `kind: model` requires `model` and an `ollama`/`openai` credential;
     - `server.models.private_endpoints` entries are `host:port`.
   - `AgentEgress` for `kind: model`: no built-in provider hosts. It adds
     the connection's host and port (port from the URL, otherwise 80/443).
     `AllowPrivate` is true and `NoLinkLocal` is true when that `host:port`
     is in `private_endpoints`.
   - **Overlay check** (`checkOverlay`): a portal `credentials` item with
     `provider: ollama|openai` whose host resolves to a private, loopback
     or link-local address is refused, unless its `host:port` is in
     `private_endpoints`.
     - Error: `this endpoint is on a private network; add "host:port" to server.models.private_endpoints in siphon.yaml`.
     - Resolve with a 2 s timeout. If the name doesn't resolve, refuse it
       too (fail closed, with the same message).
   - Schema regenerated.
   - **Tests:** validation table; `AgentEgress` for model agents (public,
     listed private, unlisted private); overlay refusal and acceptance.
2. **Guard.**
   - `source.ResolveAllowedMode(ctx, host, mode)`, where the modes are
     `public`, `private` (the existing `allowPrivate=true`) and
     `privateNoLinkLocal`. Keep `ResolveAllowed` as a wrapper.
   - `egress.Entry` gains `NoLinkLocal bool`, and the proxy passes the
     right mode. `config.HostPort` gains the same flag, and
     `internal/job/egress.go` maps it.
   - **Tests:** `169.254.169.254` and `fd00:ec2::254` are refused in
     `privateNoLinkLocal`, while `10.x` and loopback are allowed; the proxy
     honours the flag end to end.
3. **Agent loop.**
   - `internal/agentloop`:
     - `type Spec struct{ BaseURL, Model, KeyFile, Prompt string; MCP map[string]MCPServer; Allowed []string; MaxTurns int; MaxCostUSD float64 }`;
     - `Run(ctx, Spec, stdout io.Writer) int`.
   - **HTTP:** a `Transport` with `Proxy: nil` and a `DialContext` that,
     when `HTTPS_PROXY` is set, dials the proxy, sends
     `CONNECT host:port` with `Proxy-Authorization` from the URL userinfo,
     and checks the 200 response. TLS goes on top for https targets.
     Without `HTTPS_PROXY` it dials directly (`sandbox: none` testing).
   - **MCP:** reuse the go-sdk client, as `internal/source/mcp.go` does.
     For each server, list the tools and keep the allowed ones. The tool
     name sent to the model is `mcp__<server>__<tool>`, and the call is
     routed back to its server.
   - **Loop:**
     - decoding tolerates both `tool_calls` and `function_call`, and
       ignores unknown fields;
     - a malformed tool call ends the run with an error message;
     - the transcript is capped at 512 KiB, trimming the oldest tool
       results first.
   - **Output:** the JSON line above. The exit code is non-zero on errors.
   - `cmd/siphon`: a hidden `agent-run <spec.json>` reads the spec file,
     which sits in the run's private `/tmp`.
   - `internal/action/runner_model.go`: `buildRun` for `kind: model`
     writes `spec.json` and the key file into `Files`, with argv
     `[siphonExe, "agent-run", <path>]`. `siphonExe` is
     `os.Executable()`, which inside the unit resolves to the store path.
     Mask the key.
   - **Tests:**
     - an `httptest` OpenAI stub returns one tool call, then the final
       answer, against an in-process go-sdk MCP server; the result is
       printed;
     - the plain fallback (the stub returns 400 for `tools`);
     - turn cap;
     - a disallowed tool is never sent;
     - the CONNECT dialer against the real `internal/egress` proxy, for an
       http and an https `httptest` server;
     - the key never appears in output or errors.
4. **Web** (Opus).
   - Rename the page, add the redirect and update the nav.
   - The Models section, tiles and form (name, URL, key).
   - Test and model-list endpoints, using the guard and allowlist, with a
     60 s cache and an id cap.
   - The agent editor: a `kind` option "Model (built-in loop)" that shows
     the connection and model pickers through htmx.
   - Badges: Ollama (llama-ish monogram "O") and OpenAI-compatible ("{ }").
   - The run list shows the model for model agents.
   - **Tests:**
     - the pages render;
     - test and models need a session and CSRF;
     - an unlisted private URL is refused in the form;
     - the test output has no body or key;
     - the CSP template test passes.
5. **NixOS** (Opus).
   - The module option and its default from `services.ollama`.
   - VM test: the `external` node gets a Python stub that serves the
     OpenAI-compatible endpoints and a minimal streamable-HTTP MCP server
     (`initialize`, `tools/list`, one `echo` tool, `tools/call`). The test
     config has a model connection (`openai`, `url: http://external:8000/v1`,
     listed in `private_endpoints`) and a `kind: model` agent with that MCP
     source and the allowed `mcp__ext__echo`.
   - Assert the job finishes `done`, its output has the final answer, the
     stub saw the `tools/call`, and the egress for the run allowed only the
     listed hosts.
   - All existing subtests stay green.
6. **Live and PR** (Opus).
   - On p620: `server.models.private_endpoints: ["127.0.0.1:11434"]` in
     `~/.local/state/siphon-dev/siphon.yaml`, a connection to the local
     Ollama, Test (lists `qwen3.8:27b`), then a `kind: model` agent answers
     a rule.
   - A fresh Opus security review: the CONNECT dialer, the guard mode,
     SSRF via test/models, key handling, and the overlay check.
   - README "Model connections".
   - The PR.

## Tests

`go test -race ./...`, `devenv test`, `nix build .#checks.x86_64-linux.vm`
(all subtests), and the live Ollama run.

## Handoff

- **Coder:** steps 1 → 2 → 3, one agent, each later step sent by
  `SendMessage`.
- **Opus:** steps 4–6. Step 4 can start once step 1 is committed.

## Rollback

Revert the merge. Existing claude/codex/agy configs are untouched, and only
the new providers and `kind: model` disappear.

## Deviations log
- **Approval (2026-10-07):** the owner replied "merge and continue" to "Approve the plan and I'll start". The plan is recorded as approved on that reply.
- **Step 1 (coder):**
  - `private_endpoints` entries need an explicit port and no wildcards.
  - New helpers: `config.ModelURL` (parses, refuses userinfo, defaults the port) and `(*Config).PrivateEndpoint`.
  - The overlay check refuses a host when any resolved address is private.
  - `api_key_file` on a model agent falls out through the provider check.
- **Step 4 (Opus):**
  - `/connections` serves the existing logins template, extended with the Models section. `GET /logins` redirects with 301 and keeps the query. The `POST /logins…` form actions stay as they are.
  - Test and model listing run in the daemon:
    - the existing guard, with `allowPrivate` only for listed endpoints, plus an explicit link-local and metadata refusal until step 2's mode lands;
    - dials only the checked addresses, no redirects;
    - output is model ids only, with errors classified (no bodies or addresses);
    - a 60 s cache.
  - The agent editor's `model` field suggests models from the chosen connection (`GET /connections/models-for`, htmx datalist). The `kind` select gains `model`.
  - `app.js` sets the URL placeholder from the selected tile (progressive). An empty URL uses the preset's default on the server.
- **Step 2 (coder):**
  - Modes are `source.Public`, `Private` and `PrivateNoLinkLocal`. The last also refuses multicast link-local and the metadata addresses `fd00:ec2::254`, `168.63.129.16` (Azure) and `100.100.100.200` (Alibaba).
  - The proxy's pass case is tested with loopback, since a `10.x` address would hang on dial.
  - The web test and model-list code switched to `PrivateNoLinkLocal` (Opus).
- **Step 3 (coder; one fix by Opus):**
  - `allowed_tools` also accepts `mcp__<server>` for a whole server, as in Claude Code.
  - A 4xx on a request with `tools` (except 401, 403 and 429) retries once without them.
  - `max_turns` defaults to 20.
  - Lane deviations, needed: `AgentOptions.Model` and `BaseURL` plus `case "model"` in `internal/action/agent.go`, and `case c.IsModel()` in `internal/job/pipeline.go`, so keyless Ollama doesn't take the subscription path.
  - Config accessors `Credential.IsModel` and `BaseURL` (adds `/v1` for ollama).
  - Opus: an auth failure on a model agent now says "connection … was refused (bad or missing API key)" instead of the CLI re-login message.
- **Step 5 (Opus):**
  - `services.siphon.models.privateEndpoints` defaults to the local `services.ollama` (`127.0.0.1:<port>`). It's merged into the settings with `recursiveUpdate`; a shallow `//` was caught before it ran.
  - The VM stub is `nix/stub-model.py`, a stdlib OpenAI-compatible endpoint and minimal streamable-HTTP MCP server.
  - The subtest passed the first time: the model asks for the tool, the loop calls it through the proxy, and the final answer is checked; the stub records the call.
  - Live on p620 (`sandbox: none` dev instance): a `kind: model` agent on the local Ollama `qwen3.8:27b` answered "The capital of Norway is Oslo." (1 turn, 64 s).
  - Also fixed: `validate` no longer warns "max_turns not enforced" for model agents.
- **Step 6, security review fixes** (fresh Opus reviewer; fixed by the coder; verified by Opus with the race tests and all 13 VM subtests):
  - **High:** a secret ref kept from the file can't move to another provider or URL. The same rule covers source secrets and headers.
  - **Medium:** model URLs refuse `?`, `#` and dot segments. An openai path must be empty or end in `/v1`, and an ollama path must be empty.
  - **Medium:** the private-endpoint DNS check runs only at commit, for changed credentials. Load, `validate` and startup do no DNS. Restored revisions skip it, since every item passed it when first saved, and the runtime guard still applies.
  - **Tool calls:** at most 16 per turn, with trimming after each result.
  - **Name collisions:** a duplicate full tool name fails the run, and `__` is refused in the names of MCP sources used by model agents.
  - **Guard:** `PrivateNoLinkLocal` also refuses NAT64-embedded link-local or metadata addresses, unspecified addresses and multicast.
  - **Form:** the credentials form has `ollama`/`openai` and `url`, so Edit and key rotation work.
  - **Cache:** model-list caching is bounded to real connections and cleared on credentials commits.
