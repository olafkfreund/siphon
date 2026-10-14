---
status: approved
issue: 74
author: olafkfreund
---

# Intent: agents can read and act on ServiceNow through a built-in MCP server

## Problem

Since #73, ServiceNow can *trigger* Siphon, but nothing in Siphon can
*reach* ServiceNow:
- An agent woken by an incident can't read the record, look up the CMDB
  item, or add a work note.
- No `cmd` step can do it either, because no secret reaches a `cmd`.

There is no ServiceNow MCP server we can ship and trust:
- The official MCP Server Console needs a licensed Now Assist SKU.
- Community servers would be a stdio `command`, which is operator-only.
  They would also be a supply-chain dependency handed live ServiceNow
  credentials.

The bridge does give a packaged MCP server its credentials, a sandbox and an
egress allowlist. But ServiceNow has no package to put in it.

This is Phase 2 of the ServiceNow integration (#71).

## Proposed outcome

- **`siphon mcp-servicenow`:** a stdio MCP server built into the binary.
  - Run through the existing bridge, it exposes ServiceNow tools: get and
    search records; create, update, add a work note to and resolve
    incidents; create and get changes; look up the CMDB; and order catalog
    items (see the open questions).
- **A built-in `servicenow` MCP package,** so a source only needs
  `package: servicenow` plus the instance URL and credentials.
  - No `siphon.yaml` change is needed.
  - Egress is limited to that instance's host.
- **`siphon connect servicenow`** now also creates that MCP source, named
  `<name>`, next to #73's `<name>-hooks`.
  - Its connection test proves the credentials work before anything is
    saved, as other services' tests do.
- **One shared Go client, `internal/servicenow`,** which Phase 3's native
  `servicenow:` action (#75) reuses as is.
- **An agent with `mcp: [servicenow]`** and an allowlist of those tools can
  read an incident and add a work note under restricted egress. Writes keep
  the default approval.

## Affected users and systems

- Users who connect ServiceNow, and the agents that use it.
- ServiceNow admins, who create the OAuth client or API key and the
  integration user (#73's guide already names that user).
- `cmd/siphon` (the new subcommand, `localOnly`), `internal/config` (the
  built-in package), the new `internal/servicenow`, `internal/web` (the
  connection test), `internal/catalog/services.yaml`, and
  `docs/connections/servicenow.md`.

## Constraints

- **No basic auth.** Use OAuth client credentials or an API key
  (`x-sn-apikey`). ServiceNow is retiring basic auth for integrations.
- **No new dependency.** Use the stdlib HTTP client and the `go-sdk` that
  `siphon mcp` already uses.
- **Secrets only reach the bridge unit,** as source env values. They're
  masked, never logged, and never in an error message.
- **Under the bridge, egress goes through the proxy.** The client must honour
  `HTTPS_PROXY`, which #72 now passes through.
- **The instance URL is the user's own host.** It only gets private access if
  the operator lists it in `server.services.private_endpoints`, as with
  other `url_env` packages.
- **Agents can't exceed the integration user's ACLs.** Siphon adds no ACL
  layer of its own, and the docs say so.
- **#73's on-instance checks are still owed:** the signed delivery, a
  non-ASCII title, the loop guard, and `password2`. They're done together
  with this phase's instance check: an agent reads an incident and adds a
  work note.

## Open questions

- **Which tools ship now?** The roadmap lists 10. I'd ship 9 and **defer
  `order_catalog_item`**:
  - Ordering provisions real things (laptops, access, licences).
  - Two-step checkout varies by instance.
  - Agents rarely need it for incident work.

  The other 9 are the incident and change lifecycle, plus search and CMDB.
  Do you want ordering in this phase?
- **Auth methods:** both OAuth client credentials and the API key, as the
  roadmap says? Client credentials needs Washington DC or later, with the
  grant switched on. The API key needs an API key auth profile.
  - **I'd ship both.** The connect form asks for one or the other.
- **The tool-name prefix:** the tools appear to agents as
  `mcp__<source>__get_incident`. Is the unprefixed set (`get_incident`,
  `search_records`, …) fine, or do you want a `sn_` prefix?
  - **I'd go unprefixed.** The source name already namespaces them.

## Decisions (owner, at approval)

- **Tools:** ship 9. `order_catalog_item` is deferred.
- **Auth:** both OAuth client credentials and the API key; the connect
  form asks for one.
- **Tool names:** unprefixed (`get_incident`, `search_records`, …).
