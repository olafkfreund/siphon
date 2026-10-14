---
status: draft
issue: 110
spec: spec/2026-10-14-110-servicenow-poll.md
---

# Plan: ServiceNow record changes trigger rules without a webhook

## Approved decisions, carried over

- **No Go code.** Polling is the connection's own `servicenow` mcp source
  (`package: servicenow`) with `poll` plus `read` on `search_records`.
  - A separate polling source can't reuse the stored credentials, because
    cross-item secret refs are refused.
  - It was proven live on siphon-dev: create gave 1 fire, update gave 1
    more, and an unchanged record gave 0.
- **The read block,** exactly this (intent decision 1: every 2 minutes,
  over a 10-minute window):

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
- **The rule shape:**
  - `for_each: event` (the event is the record list);
  - `on: each`;
  - `id: 'item.sys_id + ":" + item.sys_mod_count'`;
  - `when: 'true'`, which fires on every change (intent decision 2). The
    "new only" variant, `item.sys_mod_count == "0"`, is a comment.
- **Facts the docs must state:**
  - latency is up to one interval;
  - the window must be longer than the interval;
  - at most 50 records per poll, newest first;
  - a burst of more than 50 in the window drops the oldest;
  - deletes aren't seen;
  - a record changed several times between polls fires once, at its latest
    `sys_mod_count`;
  - `sys_mod_count` is a display value (`5,551`);
  - one Table API call per interval;
  - `javascript:` in queries needs script in encoded queries, which is on by
    default.

## Steps

1. **The catalog option, `internal/catalog/services.yaml`, `servicenow`
   (`:354-400`).**
   - Add the field
     `{key: poll_incidents, label: Poll incidents, type: bool, help: 'For instances that cannot reach Siphon: check incidents every 2 minutes, instead of or as well as the Business Rule webhook.'}`.
   - In the `{{.name}}` source's `yaml`, append the read block above inside
     `{{- if .poll_incidents}} … {{- end}}`, at the source's top level.
   - Add `servicenow-incident-poll` to `templates:`.
   - **The render test, in `internal/catalog/catalog_test.go`:**
     - `poll_incidents` on gives a source with `poll: 2m` and
       `read.tool: search_records`;
     - off gives neither;
     - instance empty gives no `servicenow` source, as today.
   - Run `go generate ./internal/catalog ./docs`. The CLI flag
     `--poll-incidents` appears in `docs/cli.md`.

   → verify: `go test -race ./internal/catalog ./docs/...`.

   Traps:
   - Bool fields: unchecked means off (`render.go:316`).
   - Use `{{.poll_incidents}}` the way `{{.webhook}}` is used (`:42`).
   - Keep the YAML indentation inside the `{{- if}}` right.

2. **The template and its validation.**
   - **The new `docs/templates/servicenow-incident-poll.yaml`.** Use the
     header style of `servicenow-incident-notify.yaml`, with `needs:
     sources/servicenow` (`siphon connect servicenow … --poll-incidents`,
     or add the read block with `siphon edit sources servicenow`).
   - **The rule** is the shape above, with an ntfy `curl` action like the
     notify template. Its text is
     `"{{.item.number}}: {{.item.short_description}}"`.
   - **`cmd/siphon/describe_test.go:53`:** give the `servicenow` source
     `poll: 2m` and a `read: {tool: search_records, args: {table:
     incident}}`, so `TestTemplatesValidate` applies the template against a
     polled source.

   → verify: `go test -race ./cmd/siphon ./docs/...`.

   Traps:
   - Templates are embedded, so `go generate ./docs` must leave no diff.
   - Check whether `servicenow` is used elsewhere in `describe_test.go`
     (`:206-207`) before changing it.

3. **The docs, `docs/connections/servicenow.md`.**
   - Add the new section "Polling instead of webhooks", after section 6
     and before "Agent tools":
     - when to use it (no inbound path) and when to prefer the webhook;
     - turning it on, with the catalog field or `siphon edit` plus the
       block;
     - the facts listed in the decisions;
     - other tables (`change_request`);
     - the "new only" `when:`.
   - Add a row to "Which path when" (`:254-262`).
   - Run `go generate ./docs`, which also updates `llms-full.txt`.

   → verify: `go test -race ./docs/...`.

   Traps: never put an instance URL or credentials from the demo instance
   in the docs.

4. **The VM regression check.**
   - **`nix/stub-servicenow.py`, `do_GET`:** answer
     `/api/now/table/incident` (path without query) when the bearer is
     right, with
     `{"result": [{"number": "INC0000042", "sys_id": "<32 hex>", "sys_mod_count": "0", "short_description": "polled"}]}`.
     Count these calls in a separate `POLLS` counter, exposed at
     `GET /_polls`, and **don't** append them to `SEEN`. The existing
     assertions expect `len(seen) == 1` (`vm-test.nix:835`).
   - **`nix/vm-test.nix:215-221`:** give `sources.servicenow` `poll = "5s"`
     and the `read` block, in Nix attrset form, with `limit = 50`.
   - **Next to `sn-note` (`:432-445`):** add a rule `sn-poll` with
     `source = "servicenow"`, `for_each = "event"`, `when = "true"`,
     `on = "each"`, the id, and `action.cmd = ["echo" "polled {{.item.number}} mod {{.item.sys_mod_count}}"]`.
   - **In the ServiceNow test section** (`:820-850`), assert that:
     - a `sn-poll` job is done, with the output `polled INC0000042 mod 0`;
     - after `_polls` has risen by at least 2 more, there is still exactly
       one `sn-poll` job.
   - **Check the existing ServiceNow assertions still hold:** `sn-note`, the
     leak checks, and the `seen` counts.

   → verify: `nix build .#checks.x86_64-linux.vm -L` (needs KVM).

   Traps:
   - Time-based checks use `wait_until_succeeds` with a timeout, never a
     bare sleep for correctness.
   - The poll's bridge must reach the stub on machine loopback (`127.0.0.1:8081`),
     as `sn-note`'s does.
   - Never `pgrep -f`.

5. **Cleanup (mine, after merge).** Take the spike rule `sn-poll-spike` off
   siphon-dev. Take the `servicenow` source's `read`/`poll` off too, unless
   the owner wants polling there.

## Tests

```sh
go vet ./... && go test -race ./...
go generate ./internal/catalog ./docs && git diff --exit-code
nix build .#checks.x86_64-linux.vm -L
```

**Model split:** steps 1–4 edit files, so the `coder` agent does them one
at a time. Step 5 is mine. A fresh Opus agent reviews `git diff` against
this plan.

## Rollback

Revert the PR. The catalog field is optional and off by default, and no
existing source or rule changes shape. A user who turned polling on keeps
a valid `read`/`poll` block either way.
