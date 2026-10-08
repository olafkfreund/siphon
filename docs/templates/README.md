# Templates

Ready-made tasks for real situations. Each file is a complete apply file:

```sh
siphon template                      # list them
siphon template github-pr-review     # print one
siphon template github-pr-review > t.yaml && siphon apply -f t.yaml --dry-run
```

Each file starts with a header: what it does (`title`), its `category`,
what you must connect first (`needs`, with the `siphon connect` command),
which secrets to pass (`secrets`), and the exact `apply` command. Every
template is validated in CI against a config that provides its needs.
The portal shows the same gallery under **Help & Docs → Templates**.

## Code review & CI

| Template | What it does | Connect first |
|---|---|---|
| [`github-ci-failure`](github-ci-failure.yaml) | Diagnose a failed GitHub Actions run | `siphon connect github --webhook`, `siphon connect login claude` |
| [`github-issue-triage`](github-issue-triage.yaml) | Suggest labels and a summary for new issues | `siphon connect github --webhook`, `siphon connect model ollama` |
| [`github-pr-review`](github-pr-review.yaml) | Review every new pull request with an agent | `siphon connect github --webhook`, `siphon connect login claude` |
| [`gitlab-mr-review`](gitlab-mr-review.yaml) | Review GitLab merge requests | `siphon connect gitlab --webhook`, `siphon connect model ollama` |
| [`gitlab-pipeline-failed`](gitlab-pipeline-failed.yaml) | Push a phone notification when a GitLab pipeline fails | `siphon connect gitlab --webhook` |

## Ops & monitoring

| Template | What it does | Connect first |
|---|---|---|
| [`alertmanager-summary`](alertmanager-summary.yaml) | Summarise Prometheus alerts with a local model, then notify | `siphon connect model ollama` · webhook secret |
| [`disk-full`](disk-full.yaml) | Warn when a disk is nearly full (and again every 6 hours) | — |
| [`service-failed`](service-failed.yaml) | Get notified when a systemd service fails | — · webhook secret |
| [`upstream-status`](upstream-status.yaml) | Know when a provider you depend on has an incident | — |

## Schedules

| Template | What it does | Connect first |
|---|---|---|
| [`nightly-report`](nightly-report.yaml) | A nightly report on a schedule: fetch, summarise, notify | `siphon connect model ollama` |
| [`weekday-standup`](weekday-standup.yaml) | A weekday reminder on a schedule (07:30, Monday to Friday) | — |
| [`weekly-digest`](weekly-digest.yaml) | A weekly digest on a schedule: every Monday at 09:00 | `siphon connect model ollama` |

## Notifications & glue

| Template | What it does | Connect first |
|---|---|---|
| [`standard-webhooks`](standard-webhooks.yaml) | Receive Standard Webhooks (Svix, Clerk, Resend, …) | — · webhook secret |
| [`webhook-command`](webhook-command.yaml) | Run a command when a signed webhook arrives | — · webhook secret |
| [`webhook-to-ntfy`](webhook-to-ntfy.yaml) | Forward any webhook to your phone | — · webhook secret |

## Local LLM

| Template | What it does | Connect first |
|---|---|---|
| [`local-summariser`](local-summariser.yaml) | A reusable agent on your local model | `siphon connect model ollama` |
| [`model-connection`](model-connection.yaml) | Connect a local Ollama | — |
| [`summarise-webhook`](summarise-webhook.yaml) | Summarise any incoming payload with a local model | `siphon connect model ollama` · webhook secret |

## AWS

| Template | What it does | Connect first |
|---|---|---|
| [`aws-cloudwatch-alarm`](aws-cloudwatch-alarm.yaml) | Investigate CloudWatch alarms with an agent (EventBridge) | `siphon connect aws --name aws --servers cloudwatch --webhook`, `siphon connect login claude` |

## MCP

| Template | What it does | Connect first |
|---|---|---|
| [`mcp-build-watch`](mcp-build-watch.yaml) | Watch a build status on an MCP server | — |
| [`mcp-task-triage`](mcp-task-triage.yaml) | Triage each failed task on an MCP server with an agent | `siphon connect login claude` |

## Homelab

| Template | What it does | Connect first |
|---|---|---|
| [`homeassistant-event`](homeassistant-event.yaml) | React to a Home Assistant event | — · webhook secret |
