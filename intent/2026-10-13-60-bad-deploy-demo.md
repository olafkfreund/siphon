---
status: approved
issue: 60
author: olafkfreund
---

# Intent: a repeatable demo, "a bad deploy, start to finish"

## Problem

Siphon has about 30 templates, a microVM and a VM test, but no single story
that shows a newcomer how sources, rules, jobs, approvals and
notifications fit together. Showing it today means connecting real GitHub,
a real model and a real phone, live, where any of them can fail.

## Proposed outcome

One command starts the demo, and one script plays the story in the portal:

1. **CI fails.** A signed GitHub `workflow_run` webhook (a merged PR's CI
   failed) matches the `github-ci-failure` rule. An agent job waits for
   approval. The presenter approves it in the portal, and the diagnosis
   arrives as a notification.
2. **The site goes down.** An Uptime Kuma "down" webhook matches the
   `uptime-kuma-down` rule. A local-model summary job runs without approval
   and notifies.
3. **The next morning.** The digest routine (from `weekly-digest`, run on
   demand) summarises the night's jobs.

At each step the portal shows the event, the rule that matched, the job,
its output and the notification. `siphon why` and `siphon test --last`
work on the same events.

The demo is built from the existing templates, applied with `siphon apply`,
so it also proves that the templates work.

## Affected users and systems

- A new `demo/` directory: a `siphon.yaml`, the tasks, recorded webhook
  payloads, and a replay script that signs them the way GitHub and Uptime
  Kuma do.
- Probably a flake app (`nix run .#demo`) that starts the microVM with the
  demo config.
- Reuse of `nix/notify-recv.py`, so notifications show up somewhere visible
  without a phone.
- A page in `docs/` describing how to present it.

## Constraints

- It must run offline by default: no GitHub, no cloud model, no ntfy
  account. Live services are optional.
- No real secrets in the repo. The demo generates its own webhook secret
  and portal token on first run, as the microVM already does.
- Approvals stay on: the approval step is part of the story.
- Don't fork the templates. If a template needs a change for the demo,
  change the template.

## Open questions

1. **Which model?** `nix/stub-model.py` returns canned text, which reads
   badly on stage. Options:
   (a) the host's Ollama through `10.0.2.2:11434` when it's running,
   otherwise the stub;
   (b) teach the stub to return a believable diagnosis and summary per
   prompt;
   (c) Ollama only.
   I'd go with (a), plus (b) as a fallback.
2. **Which agent for the CI diagnosis?** `github-ci-failure` uses a Claude
   login, which isn't available offline. Offline means an agent on the
   model connection. Its tools are GitHub's MCP tools (\`get_workflow_run\`,
   \`get_job_logs\`), which can't work offline either. The demo would serve
   them from a stub MCP server with a recorded failing log, which
   \`stub-model.py\` partly does already. Is it OK to run the agent that way
   in the demo?
3. **Live mode:** add it later as an option (a real GitHub repo and phone),
   or leave it out entirely? I'd leave it out of this task.
4. **Recording:** do you also want an asciinema or GIF capture, or only a
   script you can run live?

## Decisions at approval

1. Model: the host's Ollama when it answers, else a stub with believable per-prompt answers.
2. The CI agent runs on the model connection, with GitHub's tools served by a stub MCP server and a recorded failing log.
3. No live mode in this task.
4. A script run live; no recording in this task.
