---
status: approved
issue: 64
intent: intent/2026-10-13-64-curl-data-raw.md
---

# Spec: template notifications send text as is

## Design

**1. No `-d` in any template; always `--data-raw`.** Seventeen templates
use `curl … -d <value>`:

- **Twelve put a template value first, so `@` can reach curl:**
  - the seven summary templates;
  - `uptime-kuma-down`, `sentry-new-issue`, `upstream-status` and
    `webhook-to-ntfy`, which use event fields;
  - `gitlab-pipeline-failed`, whose value starts with `{{.event…}}`.
- **Five start with literal text, so they're safe today:**
  `disk-full`, `homeassistant-event`, `weekday-standup`,
  `stripe-dispute-notice` and `service-failed`.

All seventeen switch `-d` to `--data-raw`. A single rule is easier to
test and to copy than "`-d` is fine unless…". The `-H "Title: {{…}}"`
values all start with the literal `Title:`, so curl's `-H @file` form
can't trigger, and they stay as they are.

**2. The seven summary templates send the text.**
- `-d "{{.steps.<id>.output}}"` becomes
  `--data-raw "{{.steps.<id>.json.result}}"`.
- The routine parses a step's stdout into `steps.<id>.json`
  (`internal/job/routine.go:276-281`). Both the `model` kind
  (`agentloop`, `loop.go:90`) and the `claude` kind (`claude -p
  --output-format json`) put the answer in `result`.
- Templates render with `missingkey=error` (`internal/action/cmd.go:31`).
  If the JSON were ever missing, the notify step fails visibly instead of
  sending the wrong thing.

The seven are `alertmanager-summary`, `cloudflare-notification`,
`datadog-monitor-alert`, `home-assistant-evening-check`, `nightly-report`,
`pagerduty-incident-summary` and `weekly-digest`. All are `model`, except
`cloudflare-notification`, which is `claude`.

**3. A guard test:** `TestTemplatesUseDataRaw` in
`docs/templates_test.go`. For every template's YAML, it fails if an argv
contains `-d`, `--data`, `--data-binary`, `--data-ascii` or
`--data-urlencode`. It also fails if a notify step sends
`.steps.<id>.output` for a step that runs an agent. That's a substring
check on `{{.steps.` + id + `.output}}` for each step with `agent:`.

**4. `docs/llm.md`:** one line in the task-writing guidance. A notify step
uses `curl --data-raw`, and an agent step's text is
`{{.steps.<id>.json.result}}`.

Then `go generate ./docs` (`llms-full.txt`).

## Alternatives rejected

- **Keep `-d` and escape a leading `@` in the renderer:** that hides
  curl's semantics behind a Siphon special case, and only for curl.
- **Fix only the twelve risky templates:** it leaves `-d` in templates
  users copy, and makes the test rule harder to state.
- **A template function such as `{{text .steps.x}}`:** a new feature,
  where `.json.result` already works.
- **Change the routine so `output` is the text:** it changes the meaning
  of `output` for every existing user routine. That's out of scope.

## Risks

- **Users who already applied a template keep the old form.** The PR note
  and the issue say to re-apply. Nothing breaks: the old form keeps
  working as before.
- **The `agy` kind puts its answer in `response`, not `result`.** No
  template uses `agy`. The `docs/llm.md` line says "for claude and model
  agents".
- **The #60 demo** (not merged yet) applies `weekly-digest`. Its seds touch
  only URLs, and its `CHANGE-ME`/`example.com` guard still holds. After
  both merge, its `/digest` line is plain text.

## Verification

- `TestTemplatesUseDataRaw` passes on the new templates. It is shown to
  fail by restoring one `-d` and one `.output` before committing.
- A render test in `internal/action` (a cmd argv
  `{{.steps.s.json.result}}` with an env whose `steps.s.json` is
  `{"result":"hi"}`) gives `hi`. With `json: nil`, it gives an error.
- `go generate ./docs && go vet ./... && go test -race ./...` pass.
- `curl --data-raw '@/etc/hostname' http://127.0.0.1:<nc>` sends the
  literal text `@/etc/hostname` (a manual check, noted in the PR).
