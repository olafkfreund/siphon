---
status: approved
issue: 75
spec: spec/2026-10-14-75-servicenow-action.md
---

# Plan: rules and routines write to ServiceNow without an LLM

## Approved decisions

These are carried over from the spec and the intent.

- **`ServiceNowAction{Connection, Op, Table, SysID, Fields map[string]string}`**
  on `Action` and `Step` (`yaml:"servicenow"`).
  - **Ops:** `get`, `create`, `update`, `add_work_note`, `add_comment`,
    `resolve`, `create_change`. `order_catalog_item` is deferred.
  - **`table`:** static, default `incident`, matching
    `^[a-z][a-z0-9_]{0,79}$`. It's ignored for `create_change`.
  - **`sys_id`:** a template, required for
    get/update/add_work_note/add_comment/resolve, and empty for
    create/create_change.
  - **Field names:** `^[a-z][a-z0-9_]*$`, and not `sys_*`. Values are
    templates.
  - **Per-op fields:**
    - `add_work_note`/`add_comment` need exactly `text`;
    - `resolve` needs `close_code` and `close_notes`;
    - `get` takes only an optional `fields` (a comma list);
    - `create`/`update` need at least one field;
    - `create_change` needs `type`, one of normal, emergency or standard.
  - **`connection`** is an mcp source with `package: servicenow`.
- **Cooldown:** required when a rule's action, or its routine, contains a
  write (any op except `get`). There's a new `RoutineHasServiceNowWrite`.
- **Retry:** refused on a step whose op is `create` or `create_change`.
- **Approval:** none by default. The existing rule and step `approve` is
  honoured.
- **Execution in the daemon** (`internal/job/servicenow.go`):
  - render with `action.RenderPrompt` (`missingkey=error`), then check
    `sys_id` against `^[0-9a-f]{32}$`;
  - build the client with `servicenow.ClientFromEnv`(source env) and
    `source.HTTPClient(cfg.ServiceEndpoint(instance), 30s, 4 MiB)`;
  - stdout is JSON `{op,table,sys_id,number}`, or the record for `get`, and
    the text output is `servicenow <op> <table> <number|sys_id>`;
  - errors are masked with `cfg.Secrets()`;
  - it's wired into `pipeline.go` `run()` and `routine.go` `execOnce`.
- **Portal:** it shows the action (`actionSummary`, the job view, the test
  fire). The rule form is read-only for a rule with `servicenow:`, and a
  form save must never drop it.
- **Explain entries,** a regenerated schema, two templates, a guide section
  and a mention in `docs/llm.md`.

## Steps

1. **Config types and validation, tests first.**
   - **`internal/config/config_test.go`:** a table test,
     `TestServiceNowActionValidation`. Each case is a minimal config with a
     `servicenow` mcp source (`package: servicenow`) and one rule or
     routine. Cover:
     - every op valid;
     - an unknown op;
     - a templated table;
     - a bad table;
     - `sys_id` missing on update and present on create;
     - a `sys_`-prefixed field;
     - `resolve` without `close_notes`;
     - `add_work_note` without `text`;
     - `create_change` with a bad type;
     - a connection that's an http source;
     - a write rule without cooldown (refused), and `get` without cooldown
       (accepted);
     - a routine with a write step and a rule without cooldown (refused);
     - retry on a create step (refused) and on an update step (accepted);
     - two action kinds (refused).
   - **Then the code:**
     - add the type, plus the fields on `Action` (`config.go:386`) and
       `Step` (`config.go:562`);
     - extend `validateAction` (`config.go:1614`) to count `ServiceNow` and
       call a new `c.validateServiceNow(p, a, add)`, and change the
       message to list servicenow;
     - pass `ServiceNow: st.ServiceNow` from the step loop
       (`config.go:1369`);
     - add the cooldown check next to `config.go:1230`, and the retry check
       next to `config.go:1357`;
     - add `RoutineHasServiceNowWrite` beside `RoutineHasAgent`.
   - **Verify:** `go test ./internal/config`. The schema test will then fail;
     run `UPDATE_SCHEMA=1 go test ./internal/config` once, and commit
     `schema/siphon.schema.json`.
   - **Traps:**
     - `validateAction` is called from the step loop with a literal
       `Action{…}`. Add `ServiceNow` there, or steps silently skip
       validation.
     - The explain test may require docs for new fields. Do step 5's
       explain entries here if `explain_test` fails, and report it.

2. **`internal/job/servicenow.go` and `servicenow_test.go` (new): execution.**
   - **The function:**
     `func runServiceNow(ctx context.Context, cfg *config.Config, a *config.ServiceNowAction, env map[string]any) (exit int, out string, stdout []byte)`,
     per the decisions.
     - For tests, the client's `*http.Client` comes from a package var,
       `serviceNowHTTP = func(allowPrivate bool) *http.Client { return source.HTTPClient(allowPrivate, 30*time.Second, 4<<20) }`.
     - Exit 0 is success, and -1 is a failure with the masked message.
   - **Tests** against `httptest.NewServer`, with the instance listed in
     `server.services.private_endpoints`:
     - `add_work_note` sends `PATCH /api/now/table/incident/<id>` with
       `{"work_notes":…}`, and the stdout JSON has `number`;
     - `create` sends POST and returns `sys_id`;
     - `get` returns the record JSON;
     - `resolve` sends `state: "6"`;
     - `create_change` posts to `/api/sn_chg_rest/change/normal`;
     - a rendered `sys_id` of `x^y` fails before any request (the fake
       counts requests);
     - a missing template key fails;
     - a 403 fails, and the output doesn't contain the client secret;
     - an unlisted loopback instance is refused by the guard.
   - **Verify:** `go test -race ./internal/job -run ServiceNow`.
   - **Traps:**
     - OAuth needs the fake to answer `/oauth_token.do`. Use the API key
       env in most cases, and one OAuth case.
     - Use a short `/tmp` dir if a test needs a socket. None is expected.

3. **Wiring in `pipeline.go` and `routine.go`.**
   - **`run()`** (`pipeline.go:374`): add `case pl.Action.ServiceNow != nil:`,
     which calls `runServiceNow(ctx, cfg, pl.Action.ServiceNow, pl.Env)`
     and maps the exit to done or failed.
   - **`execOnce`** (`routine.go:243`): add `case st.ServiceNow != nil:`
     with a `context.WithTimeout(ctx, timeout)`. Set `exit`, `out` and
     `stdout`. The existing JSON-capture code then fills
     `stepResult.JSON`.
   - **Tests:**
     - a rule-level job, through the pipeline test helpers (see an existing
       `run()` test in `internal/job`), finishes `done`;
     - a two-step routine, `get` → `add_work_note` with
       `text: "seen {{.steps.get.json.number}}"`, sends the rendered
       number.
   - **Verify:** `go test -race ./internal/job`.
   - **Traps:** `decodePayload`/`Payload` serialise `config.Action` as JSON.
     Make sure `ServiceNow` round-trips (a json tag isn't needed if the
     field names are exported, but check `payload` tests).

4. **Portal (`internal/web`).**
   - **Show the action:**
     - `data.go` `actionSummary`: `servicenow: <op> <connection>`;
     - `pages.go:66-90` job view: Kind `servicenow`, Target
       `<op> <table>`;
     - `editors.go:236` test fire: Kind `servicenow`, with Argv showing the
       rendered `sys_id` and `k=v` fields, rendered with the fire's env.
   - **Rule form, read-only:** find how the rule form is built
     (`editors.go:36-50`) and saved (the handler that posts rule fields).
     - When the rule has `servicenow:`, set a view flag. The template shows
       the YAML, the note "This rule has a servicenow action: edit it as
       YAML", and no Save button.
     - The save handler refuses (422, with that message) when the stored
       rule has `servicenow:`.
     - The same goes for a routine whose steps contain `servicenow:`, if
       routines have a form.
   - **Tests:**
     - a rule with `servicenow:` renders in the list with its summary;
     - its form shows the note and no Save;
     - posting the form returns 422, and the config is unchanged.
   - **Verify:** `go test -race ./internal/web`.
   - **Traps:** the CSP is strict, so no inline `style=` or `<script>`.
     `TestTemplatesAreCSPClean` must pass.

5. **Explain, templates and docs.**
   - **`internal/config/explain.go`** (around line 92): the action
     description becomes "exactly one of cmd, unit, agent, routine or
     servicenow". Add entries for `action.servicenow` and its keys
     (connection, op with its enum, table, sys_id, fields), and the same
     for steps.
   - **`docs/templates/alert-to-servicenow-incident.yaml`** (category "Ops
     & monitoring"):
     - a rule on a `standard-webhooks` source `alerts`, with header
       `# needs: sources/alerts, sources/servicenow`;
     - `servicenow: {connection: servicenow, op: create, fields: {short_description: "Alert: {{.event.title}}", urgency: "2", impact: "2"}}`;
     - `on: each`, `id: event.id` and `cooldown: 5m`.

     Add the `alerts` source to `cmd/siphon/describe_test.go` `baseConfig`
     if no generic standard-webhooks source exists. Reuse one if it does.
   - **`docs/templates/servicenow-change-on-deploy.yaml`** (category
     "Notifications & glue"):
     - a routine `servicenow-change-on-deploy` with steps `get`
       (`table: change_request`, `sys_id: "{{.event.change_sys_id}}"`) and
       `note` (`add_work_note`,
       `text: "Deployed {{.event.version}} ({{.steps.get.json.number}})"`);
     - a rule on a `deploys` webhook source with `cooldown: 1m`.
   - **Register both** in `docs/templates/README.md`, and add them to the
     catalog entry's `templates:`.
   - **`docs/connections/servicenow.md`:** a section "Writes without an
     agent", per the spec.
   - **`docs/llm.md`:** one line in the action list.
   - Run `go generate ./docs`.
   - **Verify:** `go test ./docs ./cmd/siphon ./internal/config ./internal/catalog`.
   - **Traps:**
     - No templated argv starts with `{{`, though these aren't argv.
       `TestTemplatesValidate` uses `baseConfig`, so every source a
       template needs must exist there.
     - Never hand-edit the generated files.

6. **All checks and the instance check (mine).**
   - Run `go vet ./... && go test -race ./...`.
   - Build, then run against the demo instance with a throwaway config:
     - a `servicenow` mcp source from agenix (OAuth);
     - a manual-trigger rule with `add_work_note` on a test incident.

     Fire it with `siphon run-once`, or a signed local webhook. Check that
     the note lands and the job output JSON has the number. Resolve the
     test incident afterwards.
   - **Traps:** parse the agenix file as data, never `source` it.

## Tests

```sh
go test -race ./internal/config ./internal/job ./internal/web
go test ./docs ./cmd/siphon ./internal/catalog
go vet ./... && go test -race ./...
```

## Rollback

Revert the PR. Configs that use `servicenow:` then fail validation
("unknown field") until the action is removed. There's no store or
migration change.

## Implementation

Steps 1–5 edit about 15 files, so a `coder` agent does them and a fresh
Opus agent reviews them. Step 6 is mine.

## Deviations

- **Step 1:**
  - **Explain entries pulled forward.** `TestExplainCoversSchema` required
    them, so the `explain.go` entries from step 5 came in now.
  - **`get`'s `fields` value** is not checked as a comma list. Only the key
    name is checked, and the client validates each name at run time.
- **Step 4:**
  - **Where the summary shows:** the rule summary appears on `/rules`.
    `/config/rules` lists names only.
  - **Routines have no field form,** so their guard only covers a form-mode
    post. YAML-mode saves of servicenow items work, and are the intended
    edit path.
