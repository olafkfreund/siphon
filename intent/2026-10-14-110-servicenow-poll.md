---
status: draft
issue: 110
author: olafkfreund
---

# Intent: ServiceNow record changes trigger rules without a webhook

## Problem

Today, ServiceNow events reach Siphon only by the signed Business Rule
webhook (`docs/connections/servicenow.md`, sections 2–4). That needs the
instance to reach Siphon: a public URL, a tunnel, or a MID server. Many
company instances can't:
- outbound calls to unknown hosts are blocked;
- Siphon sits on a private network;
- there's no MID server.

For them, ServiceNow can't trigger anything in Siphon at all.

The roadmap's Phase 4 planned a polling fallback, but it was never built.
The original idea was an `http` source with an `x-sn-apikey` header. Since
then:
- the built-in `servicenow` MCP package has shipped (#79). It
  authenticates with OAuth client credentials or an API key, and has a
  read-only `search_records` tool (any table, an encoded query, at most 50
  records);
- an `mcp` source can poll a tool on a timer (`read: {tool, args}`,
  `poll:`), and rules can iterate a list (`for_each`) and fire once per id
  (`on: each`).

So the pieces exist. What's missing:
- **proof** that they fit together, including what an event looks like
  when the tool returns a list;
- **a template;**
- **docs** saying when to poll instead of using the webhook, and the limits.

## Proposed outcome

- **A user with only outbound access from Siphon to ServiceNow** connects
  ServiceNow as today, then applies a template. A rule then fires once for
  each new or changed incident, within a poll interval. No Business Rule
  and no inbound network path are needed.
- **The template** is `servicenow-incident-poll`:
  - an `mcp` source on the `servicenow` package that polls `search_records`
    for incidents updated in a recent window, newest first;
  - a rule that iterates them with `for_each`, keyed on
    `sys_id:sys_mod_count`, so each change fires once and an unchanged
    record never refires;
  - a cheap action: a notification, as in `servicenow-incident-notify`.
- **The docs** add a "Polling instead of webhooks" section, covering:
  - when to choose it;
  - the window and interval rule: the window must be longer than the
    interval;
  - the 50-records-per-poll cap;
  - the extra API load;
  - that deletes aren't seen.
- **Checked against the demo instance (calitiiltddemo3):** create and
  update an incident, and see the rule fire for each change, once.

If the existing pieces don't fit, this becomes the smallest Go change that
makes them fit. For example, a tool result may need to arrive as a list
event. The spec will say which.

## Affected users and systems

- ServiceNow users whose instance can't reach Siphon.
- `docs/templates/`, `docs/connections/servicenow.md` and the catalog
  entry's `templates:` list.
- If needed, the MCP read path in `internal/source/mcp.go`.

## Constraints

- **Read-only:** the poll uses `search_records` only, so no write tool is
  reachable.
- **No new credential kind,** and nothing secret in the template. It reuses
  the connection's existing `env`.
- **Bounded load:**
  - a sensible default interval (for example 1–2 minutes);
  - an explicit window in the query;
  - the cap of 50 results, which is documented, with a note on what happens
    in a burst.
- **No Go change** unless the template can't work without one.

## Open questions

1. **The default interval and window.** I'd poll every 2 minutes with a
   10-minute window: a missed poll or two is caught up, and duplicates are
   removed by the `sys_id:sys_mod_count` key. OK?
2. **Changes or creates?** Should the template fire on every change, or
   only on new incidents? **I'd fire on changes,** like the webhook
   template, and show the "new only" variant in the docs as a one-line
   `when:` change (`item.sys_mod_count == "0"`).
