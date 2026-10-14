---
status: approved
issue: 75
intent: intent/2026-10-14-75-servicenow-action.md
---

# Spec: rules and routines write to ServiceNow without an LLM

## Design

### Config (`internal/config/config.go`)

```go
// ServiceNowAction runs one ServiceNow operation in the daemon.
type ServiceNowAction struct {
	Connection string            `yaml:"connection"` // an mcp source with package: servicenow
	Op         string            `yaml:"op"`         // get|create|update|add_work_note|add_comment|resolve|create_change
	Table      string            `yaml:"table"`      // default incident; change_request for create_change
	SysID      string            `yaml:"sys_id"`     // template; get/update/add_*/resolve
	Fields     map[string]string `yaml:"fields"`     // values are templates
}
```

`Action` and `Step` each gain `ServiceNow *ServiceNowAction
\`yaml:"servicenow"\``.

**Validation** (`validateAction` and the step loop):
- **Exactly one action kind:** the message becomes "exactly one of cmd,
  unit, agent, routine, servicenow".
- **`connection`** names a source with `type: mcp` and
  `package: servicenow`.
- **`op`** is one of the seven.
- **`table`** is static and matches `^[a-z][a-z0-9_]{0,79}$`. For
  `create_change` it's ignored, and `fields.type` must be `normal`,
  `emergency` or `standard`.
- **`sys_id`** is required for `get`, `update`, `add_work_note`,
  `add_comment` and `resolve`, and must be empty for `create` and
  `create_change`. It's checked with `checkTemplate`.
- **Field names** match `^[a-z][a-z0-9_]*$` and don't start with `sys_`.
  Each value goes through `checkTemplate`.
- **Per-op rules:**
  - `add_work_note` and `add_comment` need exactly `fields.text`;
  - `resolve` needs `close_code` and `close_notes`;
  - `get` takes no fields except an optional `fields.fields`, a
    comma-separated list to return;
  - `update` and `create` need at least one field.
- **A rule whose action, or whose routine, contains a write** needs
  `cooldown > 0`: "cooldown is mandatory for servicenow writes". Each write
  bumps `sys_mod_count`, so an incoming event can't dedupe the echo. Reads
  (`get`) are exempt. The check sits next to the agent cooldown check at
  `config.go:1230`, via a `RoutineHasServiceNowWrite`, like
  `RoutineHasAgent`.
- **A step with `retry` is refused** when its op is `create` or
  `create_change`: "retry is not allowed on servicenow create (a retry would
  make a duplicate)".
- **`ConfigItems`/overlay:** the action is ordinary YAML, so the
  API/portal overlay rules need no change. The schema is regenerated.

### Execution (`internal/job/servicenow.go`, new)

`func (p *Pipeline) runServiceNow(ctx, cfg, a *config.ServiceNowAction, env map[string]any) (exit int, out string, stdout []byte)`

1. **Render** `sys_id` and every field value with
   `action.RenderPrompt(v, env)`, which is `missingkey=error`. An error
   fails the job with the template error.
2. **Check** the rendered `sys_id` against `^[0-9a-f]{32}$`. The client
   checks it again.
3. **Look up** `src := cfg.Sources[a.Connection]`, then build the client:
   `servicenow.ClientFromEnv(func(k) string { return src.Env[k].Value }, source.HTTPClient(cfg.ServiceEndpoint(instance), 30*time.Second, 4<<20))`.
   That's the guarded client the connection test uses: public hosts only
   unless listed, and no redirects.
4. **Call:**

   | op | client call |
   |---|---|
   | `get` | `Get(table, sys_id, fields)` |
   | `create` | `Create(table, fields)` |
   | `update` | `Patch(table, sys_id, fields)` |
   | `add_work_note` | `Patch(table, sys_id, {work_notes: text})` |
   | `add_comment` | `Patch(table, sys_id, {comments: text})` |
   | `resolve` | `Patch(table, sys_id, {state: "6", close_code, close_notes})` |
   | `create_change` | `CreateChange(type, rest of fields)` |

5. **Output:** stdout is compact JSON `{"op","table","sys_id","number"}`.
   For `get`, it's the record. The text output is one line: `servicenow
   <op> <table> <number|sys_id>`.
6. **Errors** run through `action.Mask(err.Error(), cfg.Secrets())`, so the
   connection's env values (the source env) are masked. The client's
   errors already carry no secrets; this is the second fence.

**Wiring:**
- **`pipeline.go` `run()`:** add a case,
  `case pl.Action.ServiceNow != nil:`, to `runServiceNow`. Done or failed
  follows from the exit code.
- **`routine.go` `execOnce`:** add the same case. Its stdout feeds
  `stepResult.JSON`, as cmd steps do, so
  `{{.steps.<id>.json.sys_id}}` works.
- **Approval** needs no new code. `cfg.NeedsApproval(rule)` and the step's
  `approve` already gate before `run()`.

### Portal and CLI

- **Show the action:**
  - `web/data.go` `actionSummary`: `servicenow: <op> <connection>`;
  - `pages.go` job view: Kind `servicenow`, Target `<op> <table>`;
  - `editors.go` test-fire: Kind `servicenow`, with the rendered `sys_id`
    and fields shown as the "argv".
- **The rule form is read-only for this action** (owner decision). If a
  rule or step has `servicenow:`, the form shows the YAML and a note,
  "edit this rule as YAML", and has no Save. A form save must never drop the
  action. A test posts the form for such a rule and gets an error, not a
  silent change.
- **`internal/config/explain.go`:** add field docs for `servicenow` and its
  keys, so `siphon explain rule` lists them.

### Templates and docs

- **`docs/templates/alert-to-servicenow-incident.yaml`:** a rule on a
  generic webhook alert creates an incident with
  `short_description: "Alert: {{.event.title}}"` and `urgency`/`impact`,
  with `cooldown: 5m`.
- **`docs/templates/servicenow-change-on-deploy.yaml`:** a routine on a CI
  webhook. Step 1, `get`, reads the change by `sys_id` from the event; step
  2, `add_work_note`, adds "Deployed {{.event.version}}". It uses
  `cooldown: 1m`.
- **`docs/connections/servicenow.md`** gains a section, "Writes without an
  agent": the YAML, the op table, cooldown, no retry on create, and the
  output JSON.
- **`docs/llm.md`** gets a short mention, and `go generate ./docs`.

## Alternatives rejected

- **A generic `http:` action with secret headers.** That's broader, and
  harder to keep secrets out of URLs and bodies. The roadmap chose a
  ServiceNow-specific action.
- **Running it in a sandboxed unit via `mcp-servicenow`.** That's more
  moving parts for one HTTP call. The daemon already holds the secret and
  the guarded client.
- **A templated `table`.** Event text could pick the table. A static table
  is reviewable in the YAML.
- **Approval by default.** That's the owner's decision: no. The action is
  deterministic.

## Risks

- **A rule that writes to the record whose events trigger it.** The
  cooldown limits that to one write per period, and #73's loop guard (the
  integration user) stops the echo on the ServiceNow side.
- **At-least-once routines:** a crash between the call and recording the
  result can repeat an `update` or a note. A `create` is refused retries
  but may still repeat on a crash restart. This is documented.
- **The 30 s timeout** holds a worker. That's the same as a slow `cmd`.
- **The portal form** being read-only for these rules is a known
  limitation, and it's documented.

## Verification

- **Config tests:** a validation table covering each rule above (accepted
  and refused), the cooldown requirement through a routine, and retry on
  create refused.
- **Pipeline tests** against an `httptest` instance:
  - a rule action `add_work_note` sends the PATCH body and finishes
    `done`, with output JSON;
  - a routine `get` → `add_work_note` uses
    `{{.steps.get.json.number}}`;
  - a bad rendered `sys_id` fails before any request;
  - a missing template key fails;
  - a 403 fails with a masked message (the secret is absent);
  - a private host is refused unless listed.
- **Portal:** the rule list summary, the job view kind, and the form
  refusing to save.
- **Docs and templates:** `TestTemplatesValidate`, the `docs` tests, and
  the schema test (`UPDATE_SCHEMA=1` once).
- **`go vet ./... && go test -race ./...`.**
- **On the demo instance:** a `servicenow:` rule fired by `siphon test` (or
  a signed webhook) adds a work note to a test incident.
