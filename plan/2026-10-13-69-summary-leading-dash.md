---
status: draft
issue: 69
spec: spec/2026-10-13-69-summary-leading-dash.md
---

# Plan: summary notifications survive a bulleted answer

## Approved decisions

These are carried over from the spec.

- **The guard stays unchanged:** `internal/action/cmd.go:40` rejects a
  templated argument that renders to a leading `-`.
- **Every templated curl body gets a literal label in front,** so it never
  starts with `{{`. A summary label ends in `\n` (a newline inside a YAML
  double-quoted string), so the bullets start on their own line.
- **The scope:** the 7 summary templates, the 5 event templates whose body
  starts with event text, `docs/tasks/routine.md`, and one sentence in
  `docs/llm.md`.
- **Out of scope:**
  - `docs/drafting.md:23`, a recorded model transcript;
  - `docs/cli.md:394`, a flag-syntax example for a made-up command.
- **The new test** is `TestTemplateArgsStartLiteral`: every argument after
  the command name in a template's rule `action.cmd` and routine step `cmd`
  must not start with `{{`. It shares the parse with
  `TestTemplatesUseDataRaw` through a helper.
- **Rejected:** a leading space, a curl exemption in the guard, a `stdin:`
  field, and prompting the model.

## Steps

1. **`docs/templates_test.go:97-146`: the failing test first.**
   - Extract the template parse in `TestTemplatesUseDataRaw` into
     `templateCmds(t, tpl) (cmds [][]string, steps map[string][]step)`,
     or the smallest helper both tests can use.
   - Add `TestTemplateArgsStartLiteral`. For each cmd, check `c[1:]` with
     `strings.HasPrefix(a, "{{")`. Fail with
     `"%s: argument %q starts with a template action: the guard rejects a value that renders to a leading dash; put a literal label first"`.
   - **Verify:** `go test ./docs -run 'TestTemplateArgsStartLiteral|TestTemplatesUseDataRaw'`.
     The new test **fails**, listing the 12 templates. The old test still
     passes.
   - **Traps:** don't weaken `TestTemplatesUseDataRaw` while refactoring
     it. Its envelope check and its `readsFile` check stay exactly as they
     are.

2. **The 12 templates: one body each, nothing else changes.**

   | File:line | New body |
   |---|---|
   | `weekly-digest.yaml:27` | `"This week:\n{{.steps.digest.json.result}}"` |
   | `nightly-report.yaml:27` | `"Overnight:\n{{.steps.summary.json.result}}"` |
   | `alertmanager-summary.yaml:30` | `"Summary:\n{{.steps.summary.json.result}}"` |
   | `pagerduty-incident-summary.yaml:19` | `"Summary:\n{{.steps.summary.json.result}}"` |
   | `datadog-monitor-alert.yaml:26` | `"Summary:\n{{.steps.summary.json.result}}"` |
   | `cloudflare-notification.yaml:30` | `"Summary:\n{{.steps.explain.json.result}}"` |
   | `home-assistant-evening-check.yaml:28` | `"Tonight:\n{{.steps.check.json.result}}"` |
   | `gitlab-pipeline-failed.yaml:14` | `"Project {{.event.project.path_with_namespace}} on {{.event.object_attributes.ref}}"` |
   | `sentry-new-issue.yaml:13` | `"Culprit: {{.event.data.issue.culprit}}"` |
   | `upstream-status.yaml:17` | `"Status: {{.event.status.description}}"` |
   | `uptime-kuma-down.yaml:12` | `"Uptime Kuma: {{.event.msg}}"` |
   | `webhook-to-ntfy.yaml:19` | `"Message: {{.event.message}}"` |

   All files are in `docs/templates/`.
   - **Verify:** `go test ./docs` passes, including step 1's test and the
     existing template tests.
   - **Traps:** keep `--data-raw` (#64). Keep the double quotes, or `\n` is
     a literal backslash-n.

3. **`docs/tasks/routine.md:17`:** change the body to
   `"Summary:\n{{.steps.summary.json.result}}"`.
   - **Verify:** `grep -n 'Summary:' docs/tasks/routine.md`.
   - **Traps:** none.

4. **`docs/llm.md:123-…`, the "Notify with `curl --data-raw`" bullet:**
   append the sentence "Start each templated argument with literal text
   (`"Summary:\n{{…}}"`): an argument that renders to a leading `-` is
   rejected."
   - **Verify:** run `go generate ./docs`, then `go test ./docs`. That
     covers the `llms-full.txt` freshness check in `docs/docs_test.go:74`.
   - **Traps:** `llms-full.txt` must be regenerated in the same commit, not
     hand-edited.

5. **All checks.**
   - Run `go vet ./... && go test -race ./...`.
   - On the #60 branch with this branch merged in, run
     `nix build .#checks.x86_64-linux.demo`: all three acts pass, and the
     digest notifies.
   - **Verify:** both commands pass.
   - **Traps:** none.

## Tests

```sh
go test ./docs -run TestTemplateArgsStartLiteral   # fails after step 1, passes after step 2
go vet ./... && go test -race ./...               # pass
nix build .#checks.x86_64-linux.demo              # on #60 + this branch: rc=0
```

## Rollback

Revert the PR. The template bodies, the test, the guide sentence and
`llms-full.txt` go back together. There's no data or schema change.
Applied tasks are the users' own and aren't touched either way.

## Implementation

Steps 1-4 edit four files or groups of files, so a `coder` agent writes
them under the model split, and a fresh Opus agent reviews them. Step 5 is
mine.
