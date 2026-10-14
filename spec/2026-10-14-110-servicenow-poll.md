---
status: draft
issue: 110
intent: intent/2026-10-14-110-servicenow-poll.md
---

# Spec: ServiceNow record changes trigger rules without a webhook

## What the spike proved (siphon-dev against calitiiltddemo3, 2026-10-14)

With no code change:
- the `servicenow` connection's own mcp source (`package: servicenow`) was
  given `poll` and `read: {tool: search_records, args: …}`;
- a rule was added with `for_each: event`, `on: each` and
  `id: 'item.sys_id + ":" + item.sys_mod_count'`.

Then:
- **INC0010265 created** (mod 0): one fire within a poll;
- **INC0010265 updated** (mod 1): one more fire;
- **later polls with no change:** no fires;
- **another incident changed in the window** (INC0010060): one fire, never
  repeated.

**Facts this rests on:**
- `search_records` returns a JSON array (`internal/servicenow/mcp.go:72-77`,
  `:103-109`). The MCP poll decodes that `TextContent` into the event
  (`internal/source/mcp.go:87-88`), so `event` is the list of records.
- `sys_mod_count` arrives as a display value with a thousands separator,
  such as `5,551`. It's still unique and stable as a key part. `== "0"`
  still works for "new only".
- A `package:` mcp source may carry `read` and `poll`. Only AWS sources are
  refused (`internal/config/config.go:1749-1750`).
- **Why the polling goes on the connection's own source:** a separate
  polling source can't reuse the connection's stored credentials, because a
  secret ref to another item's file is refused (the "theft" check in
  `internal/web/apiwrite_test.go:243-247`). On the connection's own
  source, they're reused as they are.

## Design

**No Go code.** The catalog, a template, docs and a VM regression check.

### 1. The catalog option, `internal/catalog/services.yaml` (`servicenow`, `:354-400`)

- **A new field:** `{key: poll_incidents, label: Poll incidents, type: bool, help: 'For instances that cannot reach Siphon: check incidents every 2 minutes instead of (or as well as) the Business Rule webhook.'}`.
- **When it's set** (and `instance` is set), the `{{.name}}` source's YAML
  adds:

  ```yaml
  poll: 2m
  read:
    tool: search_records
    args:
      table: incident
      query: "sys_updated_on>=javascript:gs.minutesAgoStart(10)^ORDERBYDESCsys_updated_on"
      fields: [number, sys_id, sys_mod_count, short_description, state, priority, urgency, assigned_to, sys_updated_on]
      limit: 50
  ```

  These are intent decision 1's timing: every 2 minutes, over a 10-minute
  window.
- **When it's not set:** nothing changes, and the agent-tools-only source
  stays as today.
- **The hooks source** is unchanged. The user can run both.
- **`templates:`** gains `servicenow-incident-poll`.

### 2. The template `docs/templates/servicenow-incident-poll.yaml`

It follows the style of `servicenow-incident-notify.yaml`, with a cheap
notification action and no agent:

```yaml
rules:
  - name: servicenow-incident-poll
    source: servicenow
    for_each: event
    when: 'true'                      # every change; for new incidents only: item.sys_mod_count == "0"
    on: each
    id: 'item.sys_id + ":" + item.sys_mod_count'
    action: {cmd: [curl, …ntfy…, "{{.item.number}}: {{.item.short_description}}"]}
```

- **Its header** says what's needed: `siphon connect servicenow … --poll-incidents`,
  or `siphon edit sources servicenow` for an existing connection.
- **The decision-2 variant** (new incidents only) is the comment on `when:`.

### 3. The docs, `docs/connections/servicenow.md`: a new section, "Polling instead of webhooks"

- **When to use it:** the instance can't reach Siphon (no public URL, no
  MID server). Otherwise, prefer the webhook, which is instant and adds no
  API load.
- **How to turn it on:**
  - with the catalog field;
  - or with `siphon edit sources servicenow`, for an existing connection,
    adding the block above.
- **The limits:**
  - **Latency:** up to one interval.
  - **The window must be longer than the interval.** At 10 minutes against
    2, up to four missed polls are caught up.
  - **At most 50 records per poll,** newest first. In a burst of more than
    50 changes inside the window, the oldest are missed, so narrow the
    query or shorten the interval.
  - **Deletes aren't seen.**
  - **One Table API call per interval** goes against the instance's rate
    limit.
  - **`sys_mod_count` is a display value** (`5,551`).
- **Other tables:** change `table` and the query. Changes use `change_request`.
- Add `servicenow-incident-poll` to the "Which path when" table
  (`:254-262`).

### 4. Regression check: `nix/vm-test.nix`, the existing ServiceNow subtest

- **The stub, `nix/stub-servicenow.py`:** `GET /api/now/table/incident`
  with a `sysparm_query` returns one fixed incident record. Today it only
  answers GETs it knows, so this adds a list answer.
- **The machine config** (`vm-test.nix:215-221`): the `servicenow` source
  gains `poll: 5s` and the `read:` block. A rule `sn-poll`, built like the
  template but with an `echo` action, fires on it.
- **The assertion:** a job `sn-poll` appears with `polled INC… mod 0`, and
  after two more polls there is still exactly one.

### 5. Template validation

`cmd/siphon/describe_test.go` `baseConfig` (`:59` area): the `servicenow`
source there gets the `read`/`poll` block, so `TestTemplatesValidate`
applies the poll template against a polled source.

**Also:** a catalog render test for `poll_incidents` true and false, then
`go generate ./internal/catalog ./docs`.

## Alternatives rejected

- **An `http` source with `x-sn-apikey`** (the original roadmap idea).
  - It needs an API key and an API Access Policy on the instance, which
    is a second credential.
  - `http` sources have no OAuth.
  - The tool route reuses the connection's OAuth as it is.
- **A separate polling source with its own copy of the credentials.**
  - It's refused when it points at another item's secret files.
  - Asking for the credentials again doubles them, and they can drift.
- **A real cursor** (`sys_updated_on` after the last seen value). It's
  stateful and needs Go. The window plus `on: each` keys is enough for the
  intent. A cursor stays the roadmap's "later".
- **A Go change to split the list into events.** `for_each` already does
  that.

## Risks

- **A burst of more than 50 changes in the window** drops the oldest. This
  is documented, and the fix is a narrower query.
- **`javascript:gs.minutesAgoStart(10)`** depends on the instance allowing
  script in encoded queries. It's on by default, and the spike used it. The
  docs give a fixed-date alternative.
- **The poll's key isn't stable:** a record changed more than twice between
  two polls fires once, for the latest `sys_mod_count`, not once per change.
  That's acceptable, and the docs say so.
- **The VM test runs longer** by a few seconds.
- **Hosts:** none outside the VM test.

## Verification

- `go vet ./... && go test -race ./...`
- `go generate ./internal/catalog ./docs`, with no diff afterwards.
- `nix build .#checks.x86_64-linux.vm`: the `sn-poll` assertion passes.
- **Live, already done:** the spike above, recorded on #110.
- **Cleanup:** after the PR merges, the spike rule comes off siphon-dev,
  and the `servicenow` source's `read`/`poll` stay only if the owner wants
  polling there.
