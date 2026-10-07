# Siphon user guide

Siphon watches webhooks, APIs and MCP servers, and when a condition holds it
runs a command, a routine or an AI agent, in a sandbox, with approvals.
Manage it from the **CLI**, check it in the **portal**, and keep the
baseline in **`siphon.yaml`**.

## Start here

1. **[Getting started](getting-started.md):** 10 minutes from login to a
   working agent task.
2. **[Concepts](concepts.md):** source → rule → action, agents,
   connections, approvals, and what's sandboxed.
3. **[Templates](templates/README.md):** about 20 ready-made tasks you can
   apply as-is.

## How-to, by task

| I want to… | Read | Templates |
|---|---|---|
| run something when a webhook arrives | [webhook-command](tasks/webhook-command.md) | `webhook-command`, `webhook-to-ntfy`, `service-failed`, `homeassistant-event`, `standard-webhooks` |
| act when a value crosses a threshold | [poll-threshold](tasks/poll-threshold.md) | `disk-full`, `upstream-status` |
| have an agent review pull requests | [github-pr-agent](tasks/github-pr-agent.md) | `github-pr-review`, `github-ci-failure`, `github-issue-triage`, `gitlab-mr-review` |
| watch an MCP server | [mcp-watch](tasks/mcp-watch.md) | `mcp-build-watch`, `mcp-task-triage` |
| chain steps (fetch → summarise → notify) | [routines](tasks/routine.md) | `nightly-report`, `alertmanager-summary` |
| approve or deny agent runs | [approvals](tasks/approvals.md) | |
| investigate cloud alarms | [AWS](connections/aws.md) | `aws-cloudwatch-alarm` |

## Connections

[Models (Ollama and OpenAI-compatible)](connections/models.md) ·
[Logins (Claude, Codex, agy)](connections/logins.md) ·
[GitHub](connections/github.md) · [GitLab](connections/gitlab.md) ·
[AWS](connections/aws.md)

## When things go wrong

[Troubleshooting](troubleshooting.md): `siphon why`, testing against the
last event, common errors, logs.

## Reference

- [CLI reference](cli.md): every command and flag.
- [Drafting tasks in plain language](drafting.md): `siphon draft`.
- [Siphon for AI assistants](llm.md): for Claude Code, Codex, and
  [`AGENTS.md`](../AGENTS.md).
- The [`siphon.yaml` example](../examples/siphon.yaml) and its
  [JSON schema](../schema/siphon.schema.json); `siphon explain <kind>`
  lists every field.
- For developers: [adding a source type](developing/adding-a-source.md).
