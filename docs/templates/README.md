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
| [`bitbucket-pr-review`](bitbucket-pr-review.yaml) | Review every new Bitbucket pull request with an agent | `siphon connect bitbucket --webhook`, `siphon connect login claude` |
| [`forgejo-pr-review`](forgejo-pr-review.yaml) | Review every new Forgejo pull request with an agent | `siphon connect forgejo --webhook`, `siphon connect login claude` |
| [`gitea-pr-review`](gitea-pr-review.yaml) | Review every new Gitea pull request with an agent | `siphon connect gitea --webhook`, `siphon connect login claude` |
| [`github-ci-failure`](github-ci-failure.yaml) | Diagnose a failed GitHub Actions run | `siphon connect github --webhook`, `siphon connect login claude` |
| [`github-issue-triage`](github-issue-triage.yaml) | Suggest labels and a summary for new issues | `siphon connect github --webhook`, `siphon connect model ollama` |
| [`github-pr-review`](github-pr-review.yaml) | Review every new pull request with an agent | `siphon connect github --webhook`, `siphon connect login claude` |
| [`gitlab-mr-review`](gitlab-mr-review.yaml) | Review GitLab merge requests | `siphon connect gitlab --webhook`, `siphon connect model ollama` |
| [`gitlab-pipeline-failed`](gitlab-pipeline-failed.yaml) | Push a phone notification when a GitLab pipeline fails | `siphon connect gitlab --webhook` |
| [`jira-new-issue-summary`](jira-new-issue-summary.yaml) | Summarise each new Jira issue with a local model | `siphon connect jira --webhook`, `siphon connect model ollama` |

## Ops & monitoring

| Template | What it does | Connect first |
|---|---|---|
| [`alert-to-servicenow-incident`](alert-to-servicenow-incident.yaml) | Open a ServiceNow incident for each alert, with no agent | a webhook source `alerts`, `siphon connect servicenow --instance …` |
| [`alertmanager-summary`](alertmanager-summary.yaml) | Summarise Prometheus alerts with a local model, then notify | `siphon connect model ollama` · webhook secret |
| [`azure-keyvault-secret-expiry`](azure-keyvault-secret-expiry.yaml) | Push a notification when an Azure Key Vault secret is near expiry | `siphon connect azure-eventgrid` |
| [`cloudflare-notification`](cloudflare-notification.yaml) | Explain Cloudflare notifications with an agent, then notify | `siphon connect cloudflare --webhook`, `siphon connect login claude` |
| [`datadog-monitor-alert`](datadog-monitor-alert.yaml) | Summarise Datadog monitor alerts with a local model, then notify | `siphon connect datadog`, `siphon connect model ollama` |
| [`disk-full`](disk-full.yaml) | Warn when a disk is nearly full (and again every 6 hours) | — |
| [`grafana-alert-explain`](grafana-alert-explain.yaml) | Explain a firing Grafana alert using Grafana's own tools (read-only) | `siphon connect grafana --webhook`, `siphon connect login claude` |
| [`linear-issue-from-alert`](linear-issue-from-alert.yaml) | Open a Linear issue for each firing alert | `siphon connect alertmanager`, `siphon connect linear`, `siphon connect login claude` |
| [`pagerduty-incident-summary`](pagerduty-incident-summary.yaml) | Summarise each triggered PagerDuty incident, then notify | `siphon connect model ollama`, `siphon connect pagerduty` |
| [`sentry-new-issue`](sentry-new-issue.yaml) | Push a phone notification for each new Sentry issue | `siphon connect sentry` |
| [`service-failed`](service-failed.yaml) | Get notified when a systemd service fails | — · webhook secret |
| [`servicenow-incident-summary`](servicenow-incident-summary.yaml) | Summarise each new ServiceNow incident, then notify | `siphon connect model ollama`, `siphon connect servicenow` |
| [`servicenow-incident-triage`](servicenow-incident-triage.yaml) | Triage each new ServiceNow incident with an agent, and post a work note | `siphon connect servicenow --instance …`, `siphon connect login claude` |
| [`upstream-status`](upstream-status.yaml) | Know when a provider you depend on has an incident | — |
| [`uptime-kuma-down`](uptime-kuma-down.yaml) | Push a notification when an Uptime Kuma monitor goes down | `siphon connect uptime-kuma` |

## Schedules

| Template | What it does | Connect first |
|---|---|---|
| [`home-assistant-evening-check`](home-assistant-evening-check.yaml) | An evening house check on a schedule, with Home Assistant's tools | `siphon connect home-assistant`, `siphon connect model ollama` |
| [`nightly-report`](nightly-report.yaml) | A nightly report on a schedule: fetch, summarise, notify | `siphon connect model ollama` |
| [`weekday-standup`](weekday-standup.yaml) | A weekday reminder on a schedule (07:30, Monday to Friday) | — |
| [`weekly-digest`](weekly-digest.yaml) | A weekly digest on a schedule: every Monday at 09:00 | `siphon connect model ollama` |

## Notifications & glue

| Template | What it does | Connect first |
|---|---|---|
| [`discord-ask-agent`](discord-ask-agent.yaml) | A Discord /ask command answered by a local model, posted back to the interaction | `siphon connect discord`, `siphon connect model ollama` |
| [`mattermost-command-agent`](mattermost-command-agent.yaml) | A Mattermost /ask command answered by a local model, posted back to the channel | `siphon connect mattermost`, `siphon connect model ollama` |
| [`notion-page-summary`](notion-page-summary.yaml) | Summarise Notion pages when they are created or edited | `siphon connect notion --webhook`, `siphon connect login claude` |
| [`servicenow-change-on-deploy`](servicenow-change-on-deploy.yaml) | Note a deploy on its ServiceNow change request | a webhook source `deploys`, `siphon connect servicenow --instance …` |
| [`servicenow-incident-notify`](servicenow-incident-notify.yaml) | Get notified when a P1 or P2 ServiceNow incident is created | `siphon connect servicenow` |
| [`slack-mention-agent`](slack-mention-agent.yaml) | An agent that answers when someone @mentions your Slack app | `siphon connect model ollama`, `siphon connect slack` |
| [`standard-webhooks`](standard-webhooks.yaml) | Receive Standard Webhooks (Svix, Clerk, Resend, …) | — · webhook secret |
| [`stripe-dispute-notice`](stripe-dispute-notice.yaml) | Get notified when a Stripe dispute is opened | `siphon connect stripe --webhook` |
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
