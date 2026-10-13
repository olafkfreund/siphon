---
status: approved
issue: 64
spec: spec/2026-10-13-64-curl-data-raw.md
---

# Plan: template notifications send text as is

## Approved decisions

- Every `curl` in `docs/templates/*.yaml` uses `--data-raw`, never `-d`,
  `--data`, `--data-binary`, `--data-ascii` or `--data-urlencode`. That's
  17 templates. `--data-raw` never reads a file for a leading `@`.
- The seven summary templates send `{{.steps.<id>.json.result}}`, the
  agent's text, instead of `{{.steps.<id>.output}}`, its JSON envelope.
  The step ids:

  | Template | Step id |
  |---|---|
  | `alertmanager-summary` | summary |
  | `cloudflare-notification` | explain |
  | `datadog-monitor-alert` | summary |
  | `home-assistant-evening-check` | check |
  | `nightly-report` | summary |
  | `pagerduty-incident-summary` | summary |
  | `weekly-digest` | digest |

  `steps.<id>.json` is the parsed stdout (`internal/job/routine.go:276-281`).
  The `model` and `claude` kinds both put the answer in `result`.
  `missingkey=error` (`internal/action/cmd.go:31`) makes a missing value
  fail the step instead of sending the wrong thing.
- `-H "Title: …"` stays, because every value starts with the literal
  `Title:`.
- A guard test, `TestTemplatesUseDataRaw`.
- One line in `docs/llm.md`.
- No daemon changes.

## Steps

1. **Templates.**
   - In all 17 files listed by `grep -ln -- ', -d,' docs/templates/*.yaml`,
     replace the argv element `-d,` with `--data-raw,`.
   - In the seven above, replace `{{.steps.<id>.output}}` with
     `{{.steps.<id>.json.result}}`.
   - Verify:
     - `grep -c -- ', -d,' docs/templates/*.yaml | grep -v ':0'` prints
       nothing;
     - `grep -l 'json.result' docs/templates/*.yaml | wc -l` is 7;
     - `go test ./docs/ ./cmd/siphon` passes.
   - Traps: only the argv element changes, so leave the comment headers
     alone. Template names must stay unique (`TestTemplateNamesAreUnique`).

2. **`docs/templates_test.go` (after `TestTemplateNamesAreUnique`, line
   92): add `TestTemplatesUseDataRaw`.**
   - Parse each template's YAML (`yaml.v3`, as the test above does) into
     agents, routines and rules.
   - Fail on any argv element in a rule's or routine step's `cmd` that is
     `-d`, `--data`, `--data-binary`, `--data-ascii` or `--data-urlencode`.
   - For each routine step with `agent:`, fail if any other step's argv
     contains `{{.steps.<id>.output}}`.
   - Verify: it passes. Then restore one `-d,` in one template and one
     `.output` in one summary template, and see both failures. Revert both,
     and don't commit them.
   - Traps: argv is a YAML sequence, so walk the parsed `cmd`, not the raw
     text.

3. **`internal/action/cmd_test.go` `TestRender` (lines 24-39): two more
   cases.**
   - `Render([]string{"curl","--data-raw","{{.steps.s.json.result}}"},
     map[string]any{"steps": map[string]any{"s": map[string]any{"json":
     map[string]any{"result": "hi"}}}})` → `argv[2] == "hi"`.
   - The same with `"json": nil` → an error.
   - Verify: `go test ./internal/action -run TestRender`.
   - Traps: none.

4. **`docs/llm.md`, "Rules that commonly go wrong" (after the argv bullet,
   line 122): one bullet.**
   - The text: "**Notify with `curl --data-raw`, never `-d`:** `-d @x` reads
     a file. A claude or model agent step's text is
     `{{.steps.<id>.json.result}}` (`.output` is its JSON envelope)."
   - Then `go generate ./docs`.
   - Verify: `go test -count=1 ./docs/`.
   - Traps: `llms-full.txt` is generated, so don't edit it by hand.

5. **The full run.**
   - `go vet ./... && go test -race ./...`.
   - `curl -s --data-raw '@/etc/hostname' http://127.0.0.1:<port>` against
     a `nc -l` listener receives the literal `@/etc/hostname`. Note it in
     the PR.
   - Traps: use the listener's PID to stop it, never `pkill -f`.

## Tests

```sh
go generate ./docs && git diff --exit-code llms-full.txt
go vet ./... && go test -race ./...
```

## Rollback

Revert the PR. It changes templates, two tests and one doc line, with no
code or schema changes.
