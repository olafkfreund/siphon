# Model endpoints (Ollama and OpenAI-compatible APIs)

A model endpoint lets agents run on **Siphon's own agent loop** (`kind: model`).
You need no subscription CLI: a local Ollama costs nothing and keeps data on
your machine.

## CLI

```sh
siphon connect model ollama --name ollama-local                       # default URL http://127.0.0.1:11434
siphon connect model openrouter --name openrouter --api-key @key.txt  # hosted: key from a file
siphon connect model openai --name vllm --url http://gpu-box:8000/v1  # anything OpenAI-compatible
```

Each one ends with a test, and lists the models it found:

```text
connected model endpoint "ollama-local"
test: ok, 14 models: gemma4:12b, gemma4:26b, qwen2.5-coder:14b, qwen2.5:7b, …
```

| Preset | Default URL | Key |
|---|---|---|
| `ollama` | `http://127.0.0.1:11434` | no |
| `lmstudio` | `http://127.0.0.1:1234/v1` | no |
| `openrouter` | `https://openrouter.ai/api/v1` | yes |
| `groq` | `https://api.groq.com/openai/v1` | yes |
| `mistral` | `https://api.mistral.ai/v1` | yes |
| `openai` | (give `--url`) | usually |

Keys are read only from `@file` or stdin (`--api-key -`), and stored
write-only.

**Re-test any time:** `siphon test model ollama-local`.

## Private addresses

An endpoint on this host or your LAN is a private address. To stop a portal
user pointing agents at internal services, Siphon only accepts the ones the
operator lists in `siphon.yaml`:

```yaml
server:
  models:
    private_endpoints: ["127.0.0.1:11434", "gpu-box:8000"]
```

The NixOS module adds a local `services.ollama` automatically. Link-local and
cloud metadata addresses are never allowed.

## Using it

```yaml
agents:
  summarise:
    kind: model
    credential: ollama-local
    model: qwen2.5:7b          # one of the models the test listed
    prompt: "Summarise in one sentence: {{.event}}"
    max_turns: 2
    timeout: 5m
```

Templates that use it: `local-summariser`, `summarise-webhook`,
`github-issue-triage`, `gitlab-mr-review`, `alertmanager-summary`,
`nightly-report`.

## Verify in the portal

**Connections → Models** shows each endpoint, its models, and a **Test**
button.

## YAML (siphon.yaml)

```yaml
credentials:
  ollama-local: { provider: ollama, url: "http://127.0.0.1:11434" }
  openrouter:   { provider: openai, url: "https://openrouter.ai/api/v1", api_key: file:/run/credentials/siphon.service/openrouter }
```
