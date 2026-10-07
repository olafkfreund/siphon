---
status: draft
issue: 20
author: olafkfreund
---

# Intent: Local and remote model providers (Ollama, OpenAI-compatible endpoints)

## Problem

Siphon's agents run only on three hosted subscriptions: Claude (Claude Code),
Codex (ChatGPT) and Antigravity. On the Logins page, Claude, Codex and agy are
the only choices. This leaves out the people the owner wants to reach:

- **Local models.** People who run Ollama, LM Studio, vLLM or a llama.cpp
  server, on the same machine or on the LAN, can't point an agent at them.
  Local models are free per run, keep data on the network, and work offline,
  which suits a gateway that reacts to events all day.
- **Other hosted providers.** OpenRouter, Groq, Mistral, Together and Azure
  OpenAI, or any "OpenAI-compatible" API, are only reachable through
  per-agent hacks, if at all.
- **Fit with the rest of Siphon.** The Logins, egress, sandbox and approval
  features assume a provider is one of the three built-ins.

## Proposed outcome

1. **Model connections on the Logins page.**
   - Alongside Claude, Codex and agy, the page offers:
     - **Ollama**: a URL such as `http://127.0.0.1:11434` or a LAN host, no key;
     - **OpenAI-compatible**: base URL, optional API key, and a model list
       (covers LM Studio, vLLM, llama.cpp, OpenRouter, Groq, Mistral,
       Together and Azure OpenAI);
     - optionally a few presets that only pre-fill the URL (OpenRouter, Groq,
       Mistral, LM Studio).
   - A connection has a name, its endpoint, a stored key if needed
     (write-only, as today), and a **Test connection** button. The test lists
     the available models, shows the round-trip time, and reports a clear
     error if the endpoint can't be reached.
2. **Agents can use a connection and a model.** An agent picks a connection
   and a model, for example `ollama-lan` with `qwen3-coder:30b`, from
   pickers filled from the connection's model list. Everything else about
   agents stays the same: the prompt, the MCP tools allowlist, turn and
   budget limits, approval, and the job page showing what will run.
3. **The same safety as today.**
   - Runs stay in the sandbox.
   - The egress allowlist for such an agent includes exactly its
     connection's host:port. Private/LAN endpoints are allowed only for that
     host, the same way `allow_private` works for MCP sources.
   - Keys never appear in pages, diffs, logs or job output.
4. **It works end to end and is tested.**
   - An Ollama-backed agent can answer a rule's prompt and call an allowed
     MCP tool, in the NixOS VM test (a small local model, or a stub
     OpenAI-compatible server in the VM).
   - One owner-run check against a real Ollama on the LAN.
5. **It is documented.** The README gains a "Model connections" section:
   Ollama, OpenAI-compatible APIs, the presets, the egress behaviour, and
   which models handle tool use well.

## Affected users and systems

- **Operators** who self-host models or use providers other than the big
  three.
- **The repo:**
  - `internal/config` (a new connection kind and an agent's
    connection/model);
  - `internal/action` (how an agent run is launched for these
    connections);
  - `internal/cred` and `internal/web` (Logins, the agent editor, the test
    button);
  - the egress allowlist assembly;
  - the VM test and the README.
- **On the owner's network:** a local Ollama, for one manual check.

## Constraints

- **The agent sandbox, egress restriction and approvals must apply exactly as
  they do today.** A connection must not become a way around them (for
  example, a connection URL can't be used as a general-purpose
  private-network fetch).
- **No large new dependencies.** The OpenAI-compatible chat API is small, and
  stdlib HTTP is enough.
- **Keys stay write-only and are stored like other secrets.** A connection
  with no key (local Ollama) must work.
- **Built for the agent's job.** Small local models often struggle with tool
  calling. The UI should say which models are known to handle tools, and a
  run that can't use tools should still work for plain answers.

## Open questions

1. **What runs the agent loop for these connections?**
   - (a) **Reuse the Codex CLI.** It supports Ollama (`--oss`) and custom
     OpenAI-compatible providers through its config. That means no new agent
     code, but it ties these agents to Codex's behaviour and tools, and to
     its idea of providers.
   - (b) **A small built-in agent loop in Siphon.** It speaks the
     OpenAI-compatible chat API with tool calls and exposes the agent's
     allowed MCP tools. It would be about 300–500 lines, run inside the same
     sandbox unit, give full control, and support any OpenAI-compatible
     endpoint the same way.
   - (c) **Both:** the built-in loop by default, with Codex as an option.

   Proposal: **(b)**. One small, testable loop under our own sandbox and
   egress rules, and it works the same for Ollama and every hosted
   OpenAI-compatible API.
2. **Anthropic-compatible endpoints** (for example via LiteLLM): include
   them now by running Claude Code with `ANTHROPIC_BASE_URL`, or leave them
   for later? Proposal: later.
3. **Presets:** which ones to ship? Proposal: Ollama, LM Studio, OpenRouter,
   Groq and Mistral (each only pre-fills the base URL), plus a generic
   OpenAI-compatible entry.
4. **Model discovery:** list models live from the endpoint (Ollama's
   `/api/tags`, OpenAI's `/v1/models`) when editing an agent, or only on
   "Test connection"? Proposal: live in the editor, cached for a minute.
5. **Naming on the page:** keep "Logins", or rename it "Connections" now
   that it covers model endpoints too? Proposal: "Connections".
