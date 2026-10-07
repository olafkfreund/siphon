# Drafting tasks in plain language

Describe what you want; a model connection writes the task; Siphon checks
it. Nothing is applied until you say so.

```sh
siphon draft "When GitHub's status page reports an incident, send me a phone notification via ntfy topic siphon-olaf-alerts. Check every 2 minutes."
```

```text
sources:
  github-status:
    type: http
    url: https://www.githubstatus.com/api/v2/status.json
    poll: 2m

rules:
  - name: github-degraded
    source: github-status
    when: 'event.status.indicator != "none"'
    on: edge
    action:
      cmd: [curl, -fsS, -H, "Title: GitHub incident", -d, "{{.event.status.description}}", https://ntfy.sh/siphon-olaf-alerts]

Changes:
+  github-status: …
+  - name: github-degraded …
```

That came from a local `qwen2.5-coder:14b` in 34 seconds. It was valid on
the first try, because Siphon gave the model the matching template.

## When a connection is still missing

```sh
siphon draft "When a GitHub pull request is opened, have an agent on my local ollama model qwen2.5:7b summarise the PR title and description in three bullets."
```

```text
agents:
  pr-summarizer:
    kind: model
    credential: ollama-local
    model: qwen2.5:7b
    prompt: |
      Summarize the pull request in three bullets:
      - Title: {{.event.pull_request.title}}
      - Description: {{.event.pull_request.body}}
    max_turns: 5
    timeout: 5m

rules:
  - name: summarize-pr
    source: github-hooks
    when: 'headers["x-github-event"] == "pull_request" && event.action == "opened"'
    on: each
    id: event.pull_request.id
    cooldown: 1m
    action: { agent: pr-summarizer }

To do:
  - source github-hooks does not exist yet: run `siphon connect github --webhook` (creates github and github-hooks)
  - agent runs wait for approval: `siphon get approvals`, then `siphon approve <job>`
```

GitHub isn't connected yet, so the draft uses the conventional source name
`github-hooks` as a **placeholder**, tells you the command that creates it,
and refuses `--apply` until it exists. Run `siphon connect github --webhook`,
then run the draft again with `--apply`. Siphon also warns about, and has
the model repair, a rule that reads GitHub's headers from a webhook that
isn't GitHub's.

## How it works

1. **Siphon builds a prompt** from:
   - your request;
   - [the rules for assistants](llm.md);
   - the field reference for the kinds involved;
   - the two or three [templates](templates/README.md) that best match
     your words;
   - the names of what already exists (sources, agents, connections).

   It **never** includes secrets.
2. **A model connection writes an apply file.** By default it's your first
   model connection: a local Ollama works, and costs nothing.
3. **Siphon validates it** exactly like `siphon apply --dry-run`. If it's
   wrong, the errors go back to the model, for up to 3 rounds.
4. **You get:**
   - the YAML;
   - the diff against your live config;
   - any errors that remain;
   - a **to-do** list: secrets to pass, connections to make first, and
     the approval reminder for agents.

## Applying

```sh
siphon draft "…" --apply           # show the diff, ask, then apply
siphon draft "…" > task.yaml       # or save it, edit it, then: siphon apply -f task.yaml
```

- `--apply` never applies a draft that still has errors (exit 3).
- If the task needs a webhook, a secret is generated, sent with the apply,
  and **shown once**.

## Choosing the model

```sh
siphon draft "…" --connection ollama-local --model qwen2.5-coder:14b
```

Code-tuned models write better YAML. Bigger is better, but a 7–14B local
model handles most tasks, especially ones close to a template.

## Review it like any change

A model can be confidently wrong. Siphon catches anything **invalid**,
but not everything **unwise**. Check:

- **The trigger is the right source.** A GitHub rule should use
  `github-hooks`, not some other webhook you happen to have.
- **`when:` is narrow enough** (not `'true'` on a busy source).
- **Agent tools are the minimum needed,** and read-only where possible.
- **Agents keep `approve: true`.** The draft never turns approval off
  unless you asked for that.

Every draft is in the audit log (`siphon get audit --event draft`), with
its request and model.

## From an AI assistant

`siphon mcp` has a `draft` tool. It returns the same result, and **never**
applies, even with `--allow-write`. The assistant shows you the diff, then
calls `apply`.
