---
status: draft
issue: 75
author: olafkfreund
---

# Intent: rules and routines write to ServiceNow without an LLM

## Problem

Since #74, an *agent* can update ServiceNow through the built-in MCP server.
Every write then goes through a model, even when nothing needs deciding: "an
alert fired, so open an incident", or "a deploy finished, so add a work note
to the change".
- That costs money and time.
- It's non-deterministic: the model may phrase it differently, or skip it.
- It puts untrusted event text in front of a model holding write tools.

A plain `cmd` step can't do it either, because no secret reaches a `cmd`
(`job/pipeline.go`). So today there's no deterministic way to write to
ServiceNow from a rule or a routine.

This is Phase 3 of the ServiceNow integration (#71).

## Proposed outcome

- **A `servicenow:` action** on a rule, and as a routine step, that runs one
  ServiceNow operation from templated values:

  ```yaml
  action:
    servicenow:
      connection: servicenow        # the mcp source from `siphon connect servicenow`
      op: add_work_note
      table: incident
      sys_id: "{{.event.sys_id}}"
      fields: { work_notes: "Alert {{.event.alert}} fired" }
  ```

- **The operations:** `get`, `create`, `update`, `add_work_note`,
  `add_comment`, `resolve` and `create_change`, the same set the agent tools
  cover. `order_catalog_item` stays deferred.
- **The result is JSON** (`op`, `table`, `sys_id`, `number`), so a later
  routine step can use `{{.steps.<id>.json.sys_id}}`. A `get` returns the
  record.
- **It runs in the daemon,** with the shared `internal/servicenow` client,
  the connection's stored credentials, and the same egress guard as the
  connection test.
- **Writes can't loop or duplicate by accident:**
  - a rule with a write needs `cooldown:`;
  - a `create` can't be retried.
- **Templates, docs and the schema** cover it:
  - `alert-to-servicenow-incident`;
  - `servicenow-change-on-deploy`, which adds a work note to a change from a
    CI event.

## Affected users and systems

- Users writing rules and routines. The portal shows the new action kind.
- `internal/config` (the Action/Step fields, validation, schema),
  `internal/job` (the pipeline and routine execution), `internal/web` (shown
  read-only), `cmd/siphon` explain, docs and templates.

## Constraints

- **Secrets never leave the daemon,** and they're masked in job output and
  errors, as for every source env value.
- **The table and the op are static,** not templated. `sys_id` is validated
  after rendering. Event text is only ever a field *value*.
- **No new dependency,** and the `internal/servicenow` client is reused
  unchanged where possible.
- **Approval works as for other actions:** the rule's or step's `approve:`.

## Open questions

- **Should a `servicenow:` write need approval by default,** like agents?
  **I'd say no:** it does exactly what the YAML says, like a `cmd`. The rule's
  `approve: true` is there when wanted.
- **Should `get` be included?** It's a read, and useful as the first step of a
  routine. **I'd include it.**
- **Should the portal's rule editor edit this action,** or only show it?
  **I'd show it read-only now,** and edit it as YAML, as the roadmap planned.
  A form is follow-up work.
