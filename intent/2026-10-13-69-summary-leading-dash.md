---
status: approved
issue: 69
author: olafkfreund
---

# Intent: summary notifications survive a bulleted answer

## Problem

#64 made the seven summary templates send the model's text:
`--data-raw "{{.steps.<id>.json.result}}"`. Models often answer with a
bullet list, so the rendered argument starts with `-`. The argv guard in
`internal/action/cmd.go:40` rejects any templated argument that renders to a
leading dash ("argument 5 introduced leading dash"), and the notify step
fails. The summary is written, but nobody gets it.

Before #64 the body was the JSON envelope, which starts with `{`, so the
guard never fired. `checks.demo` (#60, Act 3) caught it.

## Proposed outcome

- A summary template notifies whatever the model writes, bullets included.
- The guard stays as it is: a templated value must never become a curl
  option.
- A test fails if a template puts a step's or event's text at the start of
  an argument.
- `checks.demo` passes on the #60 branch.

## Affected users and systems

- The templates `weekly-digest`, `nightly-report`, `alertmanager-summary`,
  `pagerduty-incident-summary`, `home-assistant-evening-check`,
  `datadog-monitor-alert` and `cloudflare-notification`, and the guide
  `docs/tasks/routine.md`.
- Users who applied one of these since #67 merged: their copy keeps failing
  until they re-apply it.
- The #60 demo, which is blocked on this.

## Constraints

- Don't weaken or special-case the leading-dash guard.
- No new dependency. Prefer a template-only fix over new code.
- Keep `--data-raw` (#64): no `@file` reads.

## Open questions

- **How to fix the body.**
  - A: a literal prefix in each template, e.g.
    `"Weekly digest:\n{{.steps.digest.json.result}}"`. No code, but the
    first line repeats the title.
  - B: a new `stdin:` field on cmd steps, with `curl --data-binary @-`. Clean
    message, but new code and docs.
  - **I'd take A.**
