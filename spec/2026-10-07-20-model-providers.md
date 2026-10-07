---
status: approved
issue: 20
intent: intent/2026-10-07-20-model-providers.md
---

# Spec: Local and remote model providers

## Design

### 1. Config: model connections

`credentials` gains two providers. The page and docs call them
**connections**, while the YAML key stays `credentials`, so nothing breaks.

```yaml
credentials:
  ollama-lan:
    provider: ollama
    url: http://192.168.1.20:11434      # base URL; /v1 is added for the chat API
  openrouter:
    provider: openai                   # any OpenAI-compatible API
    url: https://openrouter.ai/api/v1
    api_key: file:/run/agenix/openrouter   # optional (local servers need none)
    preset: openrouter                 # optional, UI only

agents:
  triage-local:
    kind: model                        # the built-in loop (new)
    credential: ollama-lan
    model: qwen3-coder:30b
    prompt: "…"
    mcp: [factory]
    allowed_tools: [mcp__factory__task_status]
    max_turns: 10
    timeout: 5m
```

- **Validation:**
  - `url` is required for `ollama`/`openai` and must be `http(s)://host[:port][/path]`;
  - `model` is required when `kind: model`;
  - `kind: model` must use an `ollama` or `openai` connection;
  - `api_key` follows the existing secret rules, including the portal ref
    confinement (F2).
- **Presets** only pre-fill the URL in the UI:
  - Ollama `http://127.0.0.1:11434`
  - LM Studio `http://127.0.0.1:1234/v1`
  - OpenRouter `https://openrouter.ai/api/v1`
  - Groq `https://api.groq.com/openai/v1`
  - Mistral `https://api.mistral.ai/v1`

### 2. The built-in agent loop (`siphon agent-run`)

- **Where it runs.** `kind: model` agents run the siphon binary itself,
  `siphon agent-run`, as the agent command inside the same
  `siphon-action@` sandbox, with the same egress proxy, timeout, approval
  and job page as other agents. Like other runs, the job file holds:
  - the endpoint;
  - the model;
  - the key, as a file in the run's private `/tmp`;
  - the rendered prompt;
  - the MCP servers;
  - the allowed tools;
  - the turn limit.
- **The loop.** It uses the OpenAI-compatible `POST {url}/chat/completions`,
  which Ollama also serves at `/v1`, with `tools` built from the agent's MCP
  servers:
  - It connects to the configured MCP servers with the go-sdk client, the
    same way as the existing runners' MCP config. http MCP goes through the
    egress proxy; stdio servers are spawned inside the sandbox.
  - It lists their tools and keeps only those in `allowed_tools` (names
    `mcp__<server>__<tool>`). That allowlist is mandatory, as it is for
    Claude.
  - It maps the tools to the OpenAI `tools` JSON schema.
  - It sends the system and user messages, executes the returned
    `tool_calls` through MCP (with each result capped at 64 KiB), and
    repeats up to `max_turns`.
  - It prints a final JSON result to stdout:
    `{"type":"result","result":…,"turns":n,"usage":{…}}`. That's the same
    shape the portal already shows, and agent-result events work unchanged.
- **Models without tool support.** If the endpoint rejects `tools`, or the
  agent has no allowed tools, the loop does a single plain completion and
  says so in the output.
- **HTTP through the egress proxy.** Go's transport sends plain-`http://`
  requests to a proxy in absolute form, not as CONNECT, and the Siphon proxy
  only accepts CONNECT. So the loop uses its own dialer that opens a CONNECT
  tunnel through `HTTPS_PROXY` for every target, http and https alike. No
  change to the proxy is needed.
- **Limits.**
  - `max_turns` and the timeout apply as for other agents.
  - Cost is only known when the provider reports usage. `max_budget_usd`
    applies only where usage includes a cost (for example OpenRouter), and
    otherwise is ignored with a note.
  - The loop's own memory is bounded: the conversation is capped at 512 KiB
    and the oldest tool results are trimmed.
- **Size.** About 400 lines in `internal/agentloop`, using stdlib HTTP plus
  the existing MCP SDK. No new dependencies.

### 3. Egress and the network boundary

This section keeps the rule from #15: **a portal user can't widen the host's
privileges.**

- A `kind: model` agent's allowlist is its connection's `host:port`, plus
  its MCP sources, `egress.allow` and `server.egress.allow`, as for other
  agents.
- **Public endpoints** (they resolve to public addresses) need nothing
  extra.
- **Private, loopback or LAN endpoints** (most Ollama setups) are allowed
  only when the exact `host:port` is listed in the file:
  ```yaml
  server:
    models:
      private_endpoints: ["127.0.0.1:11434", "192.168.1.20:11434"]
  ```
  - A listed endpoint is then usable from portal-made connections.
  - An unlisted private endpoint is refused at save time, with an
    actionable message that names the YAML line to add.
  - The NixOS module gains `services.siphon.models.privateEndpoints`. When
    `services.ollama.enable` is on, it defaults to
    `[ "127.0.0.1:<services.ollama.port>" ]`, so local Ollama works with no
    extra setup.
- **The proxy's private allowance for a connection is per host:port and
  never covers link-local or metadata addresses** (`169.254.0.0/16`,
  `fd00:ec2::254`). That's a new `allowPrivateNoLinkLocal` mode in the
  guard, used for connection entries.
- **Loopback endpoints:** the sandbox's own loopback is its private
  namespace, so `127.0.0.1:11434` is reached by the proxy on the host. That
  is the intended behaviour, and it's limited to the listed port.

### 4. Connections page (renamed from Logins)

- `/logins` becomes `/connections` (the old path redirects). The nav item is
  "Connections".
- Two sections:
  - **Subscriptions:** Claude, Codex and agy, unchanged.
  - **Models:** Ollama, OpenAI-compatible and the presets.
- **Adding a model connection:** provider tiles (Ollama, LM Studio,
  OpenRouter, Groq, Mistral, OpenAI-compatible) show their own icon/monogram
  and pre-fill the URL, plus name, URL and API key (optional, write-only).
- **Test connection:** `POST /connections/{name}/test` lists the models
  (`/api/tags` for Ollama, `/v1/models` otherwise) and shows the latency.
  - It runs from the daemon with the same SSRF guard and the same private
    endpoint allowlist, so the test can't be used as a network probe.
  - It returns at most 200 model ids, and never the response body.
- **Connection cards** show the provider monogram, the URL host, the model
  count, the last test result and Test/Edit/Delete. Badges for the new
  providers are original monograms in each brand's colour, not vendor logos.
- **Agent editor:** `kind` gains "Model (built-in loop)". When chosen, it
  shows a connection picker and a model picker. The model picker is filled
  live from the connection (htmx `GET /connections/{name}/models`, cached
  60 s per connection), and free text stays allowed. Known tool-capable
  model families (qwen, llama3.x, mistral, gpt-oss, command-r) get a
  "tools ✓" hint.
- **Runs and the job page** show the provider badge (Ollama / OpenAI-compat)
  and the model.

## Alternatives rejected

- **The Codex CLI with `--oss` or custom providers** (intent Q1 a): it ties
  these agents to Codex's tools, sandbox modes and provider config, and is
  harder to test and reason about. The owner chose the built-in loop.
- **Teaching the egress proxy plain HTTP forwarding:** it would add a second
  proxy mode and widen its attack surface. A CONNECT-only client dialer in
  the loop is smaller.
- **Allowing any private endpoint from the portal:** it breaks the #15
  rule, since a portal session could point agents at any LAN or loopback
  service. The file allowlist keeps it explicit, and the NixOS option keeps
  local Ollama zero-config.
- **LiteLLM as a required sidecar:** it's another service to run, while the
  OpenAI-compatible API is enough.

## Risks

| Risk | Mitigation |
|---|---|
| Small local models produce bad or no tool calls | One-shot fallback; malformed tool calls are reported in the output and end the run; turn cap; the "tools ✓" hint in the UI |
| A provider's "OpenAI-compatible" API differs (tool call shape, usage) | Tolerant decoding (ignore unknown fields, both `tool_calls` and legacy `function_call`); per-preset integration notes; the stub server in tests mirrors Ollama's actual format |
| Endpoint misuse as SSRF (test button, agents) | The same guard as sources, a private-endpoint file allowlist, link-local and metadata always blocked, test output limited to model ids |
| A key leaks in errors or output | The key is read from a file in the run's `/tmp`, never in env or argv, and masked like other secrets; HTTP errors are reported by status code and a short message only |
| A long conversation grows memory | Message history and tool results capped |

## Verification

- **Unit:**
  - config validation for the providers, `kind: model` and the
    private-endpoint rules (portal path included);
  - the loop against an `httptest` OpenAI-compatible stub: tool call, then
    MCP tool, then the final answer; the plain fallback; turn cap;
    malformed tool calls; a disallowed tool is never offered;
  - the CONNECT dialer through the real egress proxy for both http and
    https targets;
  - the model-list parsing of both APIs;
  - the "test" output never includes response bodies or keys.
- **NixOS VM test:**
  - a stub OpenAI-compatible server on the `external` node (a few lines of
    Python serving `/v1/models` and `/v1/chat/completions` with one tool
    call);
  - a `kind: model` agent with an allowed MCP tool runs in the restricted
    sandbox, calls the tool, and returns the final answer;
  - an unlisted private endpoint is refused at save.
- **Live (owner):** connect the LAN or local Ollama on p620 from the
  Connections page, Test, then run a rule with a small tool-capable model.
