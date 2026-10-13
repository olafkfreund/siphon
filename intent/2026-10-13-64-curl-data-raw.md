---
status: draft
issue: 64
author: olafkfreund
---

# Intent: templates never let event or model text become a curl file read

## Problem

`curl -d <value>` reads a file when the value starts with `@`. Twelve
templates in `docs/templates/` build their notify step as
`[curl, …, -d, "{{…}}", <url>]`. The value is either webhook data, which a
sender controls, or model output, which prompt injection in the event
controls. For example, `webhook-to-ntfy` forwards any signed body, so a
body of `@/proc/self/environ` makes curl post that file from inside the
action sandbox to the ntfy topic.

The sandbox limits what can be read, and the webhook signature limits who
can send. But the templates are what users copy, so the pattern spreads.

## Proposed outcome

- No template passes templated text to `-d` or `--data`. They use
  `--data-raw`, which sends the value as is.
- A test in `docs/templates_test.go` fails if any template puts a `{{…}}`
  value after `-d`, `--data`, `--data-binary` or `--data-urlencode`, so
  the pattern can't come back.
- The twelve templates still apply cleanly, and the notifications look
  the same.

## Affected users and systems

- The twelve templates in `docs/templates/` and `docs/templates_test.go`.
- The regenerated `llms-full.txt`.
- The #60 demo's `play.sh` seds don't touch the curl flag, so nothing
  changes there.
- Users who already applied a template keep the old form until they
  re-apply. The PR note says so.

## Constraints

- `--data-raw` needs curl 7.43 or later (2015). Every supported platform
  ships newer.
- No change to the daemon or to what users must pass in.

## Open questions

- Should `docs/llm.md` (the guide agents read) also tell agents that write
  tasks to use `--data-raw`? I'd say yes: one line.
