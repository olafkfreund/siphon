---
status: draft
issue: 69
intent: intent/2026-10-13-69-summary-leading-dash.md
---

# Spec: summary notifications survive a bulleted answer

## Design

**Rule: no argument may start with a template action.** Every templated
curl body gets a short literal label in front, so the rendered argument
starts with a letter and the guard in `internal/action/cmd.go:40` never
fires. The guard is not changed. This is option A from the approved intent.

**Scope is wider than the intent's list.** The same failure hits any body
that starts with outside text: a sender's message that begins with `-` fails
the job. Five event templates do this. The intent's rule ("a test fails if a
template puts a step's or event's text at the start of an argument") covers
them, so they're fixed too.

| File | Line | New body |
|---|---|---|
| `docs/templates/weekly-digest.yaml` | 27 | `"This week:\n{{.steps.digest.json.result}}"` |
| `docs/templates/nightly-report.yaml` | 27 | `"Overnight:\n{{.steps.summary.json.result}}"` |
| `docs/templates/alertmanager-summary.yaml` | 30 | `"Summary:\n{{.steps.summary.json.result}}"` |
| `docs/templates/pagerduty-incident-summary.yaml` | 19 | `"Summary:\n{{.steps.summary.json.result}}"` |
| `docs/templates/datadog-monitor-alert.yaml` | 26 | `"Summary:\n{{.steps.summary.json.result}}"` |
| `docs/templates/cloudflare-notification.yaml` | 30 | `"Summary:\n{{.steps.explain.json.result}}"` |
| `docs/templates/home-assistant-evening-check.yaml` | 28 | `"Tonight:\n{{.steps.check.json.result}}"` |
| `docs/templates/gitlab-pipeline-failed.yaml` | 14 | `"Project {{.event.project.path_with_namespace}} on {{.event.object_attributes.ref}}"` |
| `docs/templates/sentry-new-issue.yaml` | 13 | `"Culprit: {{.event.data.issue.culprit}}"` |
| `docs/templates/upstream-status.yaml` | 17 | `"Status: {{.event.status.description}}"` |
| `docs/templates/uptime-kuma-down.yaml` | 12 | `"Uptime Kuma: {{.event.msg}}"` |
| `docs/templates/webhook-to-ntfy.yaml` | 19 | `"Message: {{.event.message}}"` |
| `docs/tasks/routine.md` | 17 | `"Summary:\n{{.steps.summary.json.result}}"` |

In YAML flow style, `"\n"` inside double quotes is a newline, so the label
sits on its own line above the bullets.

**Test.** In `docs/templates_test.go`, add `TestTemplateArgsStartLiteral`.
It parses every template the way `TestTemplatesUseDataRaw` does: rule
`action.cmd`, plus every routine step's `cmd`. It fails on any argument
after the command name that starts with `{{`. The message names the
template and the argument, and says "starts with a template action: the
guard rejects a value that renders to a leading dash; put a literal label
first". The parse is shared with the existing test through a small helper
that returns a template's commands.

**Guide.** Add one sentence to the "Notify with `curl --data-raw`" bullet
in `docs/llm.md:123`: "Start each templated argument with literal text
(`\"Summary:\n{{…}}\"`): an argument that renders to a leading `-` is
rejected." `siphon draft` puts this guide in its prompt, so drafted tasks
follow it too. `llms-full.txt` is regenerated if a test requires it.

**Out of scope.**
- `docs/drafting.md:23` is a verbatim transcript of a model's draft. It stays
  as recorded.
- `docs/cli.md:394` (`'["notify","{{.event.msg}}"]'`) shows flag syntax for
  a made-up command, not curl, so it stays.

## Alternatives rejected

- **A leading space (`" {{…}}"`).** It's invisible, and someone will tidy
  it away. It also relies on the receiver trimming it.
- **Exempting the argument after `--data-raw` in the guard.** That puts
  curl knowledge in core code. A rule like that is easy to get wrong for
  other tools, where the next argument isn't a value.
- **A `stdin:` field on cmd steps (option B).** New code and docs for a
  problem a label solves. The intent preferred A.
- **Asking the model not to start with a bullet.** Models don't reliably
  follow that, and event text can't be prompted at all.

## Risks

- **Users who already applied an affected template** keep the old body
  until they re-apply it. The PR description says so. No migration is
  possible: the tasks are their own data.
- **The new label changes the messages slightly.** These are notifications,
  and nothing parses them.
- **The demo (#60)** asserts only notify paths (`/kuma`, `/digest`), not
  bodies, so the labels don't break it.

## Verification

- `go vet ./... && go test -race ./...` pass, including
  `TestTemplateArgsStartLiteral`.
- The new test fails on `main` before the template edits (run it first).
- `nix build .#checks.x86_64-linux.demo`, on the #60 branch with this merged
  in, passes all three acts. Act 3 notifies a bulleted digest.
- `nix flake check` passes.
