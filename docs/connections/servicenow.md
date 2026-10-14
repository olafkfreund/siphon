# ServiceNow

ServiceNow record events (incidents, changes, requests) reach Siphon as a
**signed webhook** sent by a Business Rule. Agents can also read and update
records through built-in tools: see [Agent tools](#agent-tools).

## 1. Connect

```sh
siphon connect servicenow
```

It creates the webhook source `servicenow-hooks` and shows the URL and the
key once. Copy both now.

## 2. System properties

In ServiceNow, open `sys_properties.list` and add:

| Property | Type | Value |
|---|---|---|
| `siphon.webhook.url` | string | the URL shown |
| `siphon.webhook.key` | `password2` (encrypted at rest) | the key shown |

## 3. Integration user

Siphon's own writes (the agent tools) need a user, and the Business Rule
below must know its name so those writes never echo back. Create it
now: *Web service access only*, with the roles `snc_platform_rest_api_access`
and `itil`. Put its user name in a third property, `siphon.integration_user`
(string). If unset, the script assumes `siphon.integration`.

## 4. The Business Rule

On each table to send (for example `incident`), add a Business Rule in the **Global** application scope: **After**, on
**Insert** and **Update**, with this advanced script:

```javascript
(function executeRule(current, previous) {
  // Loop guard: Siphon's own writes never echo back.
  if (gs.getUserName() == gs.getProperty('siphon.integration_user', 'siphon.integration')) return;
  var changed = [];
  var els = current.getElements();
  for (var i = 0; i < els.size(); i++) {
    var e = els.get(i);
    if (e.changes()) changed.push(e.getName());
  }
  var body = JSON.stringify({
    event: current.operation(), table: current.getTableName(),
    sys_id: current.getUniqueValue(), number: current.getValue('number'),
    sys_mod_count: current.getValue('sys_mod_count'), sys_updated_on: current.getValue('sys_updated_on'),
    changed: changed, actor: gs.getUserName(),
    short_description: current.getValue('short_description'),
    priority: current.getValue('priority'), state: current.getValue('state')
  });
  var id = current.getUniqueValue() + ':' + current.getValue('sys_mod_count');
  var ts = String(Math.floor(new GlideDateTime().getNumericValue() / 1000));
  var sig = new GlideCertificateEncryption().generateMac(
    gs.getProperty('siphon.webhook.key'), 'HmacSHA256', id + '.' + ts + '.' + body);
  var r = new sn_ws.RESTMessageV2();
  r.setEndpoint(gs.getProperty('siphon.webhook.url'));
  r.setHttpMethod('post');
  r.setRequestHeader('Content-Type', 'application/json; charset=utf-8');
  r.setRequestHeader('webhook-id', id);
  r.setRequestHeader('webhook-timestamp', ts);
  r.setRequestHeader('webhook-signature', 'v1,' + sig);
  r.setRequestBody(body);
  r.executeAsync();
})(current, previous);
```

## 5. The event shape

The rule sees these fields as `event.<name>`:

| Field | What it is |
|---|---|
| `event` | `insert` or `update` |
| `table` | the table name, for example `incident` |
| `sys_id` | the record's unique id |
| `number` | the record number, for example `INC0010001` |
| `sys_mod_count` | how many times the record has been updated |
| `sys_updated_on` | when it was last updated |
| `changed` | the names of the fields that changed |
| `actor` | the user who made the change |
| `short_description` | the record's summary |
| `priority` | `1` to `5`, as a string |
| `state` | the state value, as a string |

The delivery id is `sys_id:sys_mod_count`. A repeated delivery of one change
carries the same id, so it collapses onto one replay key; every real update
gets a new one.

## 6. Rules

```yaml
rules:
  - name: new-incident
    source: servicenow-hooks
    when: 'event.table == "incident" && event.event == "insert"'
    on: each
    id: event.sys_id + ":" + event.sys_mod_count
    action: { cmd: [echo, "new incident"] }
```

Ready-made: `siphon template servicenow-incident-notify` and
`siphon template servicenow-incident-summary`.

## Polling instead of webhooks

Use this when the instance has no path to Siphon (no outbound HTTP to it).
When it can reach Siphon, prefer the webhook: it is immediate and sends
inserts, updates and deletes.

Turn it on with the Poll incidents option (`siphon connect servicenow ...
--poll-incidents`), or add this to the connection's source with
`siphon edit sources servicenow`:

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

The event is the list of records, so a rule uses `for_each: event`,
`on: each` and `id: 'item.sys_id + ":" + item.sys_mod_count'`. See the
`servicenow-incident-poll` template. Its `when: 'true'` fires on every
change; for new incidents only, use `item.sys_mod_count == "0"`. For another
table, such as `change_request`, change `table` (and the fields).

What to know:

- Latency is up to one interval (2 minutes here).
- The window (10 minutes) must be longer than the interval.
- At most 50 records per poll, newest first. A burst of more than 50 in the
  window drops the oldest.
- Deletes aren't seen.
- A record changed several times between polls fires once, at its latest
  `sys_mod_count`.
- `sys_mod_count` is a display value (`5,551`).
- It costs one Table API call per interval.
- `javascript:` in the query needs script in encoded queries, which is on by
  default.

## Agent tools

Give `siphon connect` the instance URL and Siphon adds a `servicenow` MCP
source next to the webhook. Its server is built in: there is nothing to
install and no `mcp_packages` entry to write.

### Credentials

Use one of these.

- **OAuth client credentials** (the default).
  - Set the system property `glide.oauth.inbound.client.credential.grant_type.enabled`
    to `true`.
  - In *System OAuth → Application Registry*, create an *OAuth API endpoint
    for external clients*. Note the client ID and secret.
  - Set the integration user as the **OAuth Application User** on that
    registry entry. Without it, tokens are issued for no one.
- **An API key.** All of these are needed, or `x-sn-apikey` gets a 401:
  - a *REST API Key* record, bound to the integration user;
  - an *Inbound Authentication Profile* of type API key, using that key;
  - an *API Access Policy* on the Table API (and on the Change Management
    API if agents use `create_change`) that uses that profile.

Both act as the integration user from step 3, so its roles and ACLs bound
what an agent can do.

```sh
# OAuth: the client secret on stdin
printf '%s' "$CLIENT_SECRET" | siphon connect servicenow --instance https://dev12345.service-now.com \
  --client-id <the client id> --client-secret -

# API key: the key on stdin
printf '%s' "$API_KEY" | siphon connect servicenow --instance https://dev12345.service-now.com \
  --auth apikey --api-key -
```

A LAN instance must be listed in `server.services.private_endpoints` first.
After saving, the connection's **Test** shows the integration user.

### The tools

Agents list the source in `mcp:`; the tools are named
`mcp__servicenow__<tool>`.

| Tool | Does | Writes |
|---|---|---|
| `get_incident` | read an incident by number or `sys_id` | no |
| `search_records` | search any table with an encoded query | no |
| `get_change` | read a change request by number or `sys_id` | no |
| `cmdb_lookup` | find configuration items by name (`LIKE`) or `sys_id` | no |
| `create_incident` | create an incident | **yes** |
| `update_incident` | change fields on an incident | **yes** |
| `add_work_note` | add a work note to a record | **yes** |
| `resolve_incident` | set state 6 with `close_code` and `close_notes` | **yes** |
| `create_change` | create a normal, emergency or standard change | **yes** |

A read-only agent lists only the reading tools. Approval gates the whole
run: an agent action waits for a human before it starts (the default), and
the approver does not see what the agent will write. Keep writing agents on
the default `approve`, and limit them with `allowed_tools`.

```yaml
agents:
  incident-reader:
    kind: claude
    credential: claude-max
    mcp: [servicenow]
    allowed_tools: [mcp__servicenow__get_incident, mcp__servicenow__search_records, mcp__servicenow__cmdb_lookup]
    prompt: "Summarise incident {{.event.number}}."
```

Ready-made: `siphon template servicenow-incident-triage`.

### Writes without an agent

A rule or a routine step can call ServiceNow itself, with no LLM and no
agent budget. The daemon makes the call as the integration user, using the
`servicenow` source's credentials.

```yaml
rules:
  - name: alert-to-incident
    source: alerts
    when: 'event.title != ""'
    on: each
    id: event.id
    cooldown: 5m
    action:
      servicenow:
        connection: servicenow
        op: create
        fields: { short_description: "Alert: {{.event.title}}", urgency: "2" }
```

- **Ops:** `get`, `create`, `update`, `add_work_note`, `add_comment`,
  `resolve` and `create_change`.
- **`table`** is plain text, not a template (default `incident`; ignored by
  `create_change`). **`sys_id`** is a template that must render to 32 hex
  characters; leave it out for `create` and `create_change`.
- **`fields`** values are templates. `add_work_note` and `add_comment` take
  only `text`; `resolve` needs `close_code` and `close_notes`; `create_change`
  needs `type` (`normal`, `emergency` or `standard`); `get` takes an optional
  `fields` comma list. Field names may not start with `sys_`.
- **A write needs a `cooldown`** on its rule, or on a rule whose routine has
  one. `get` does not.
- **No retry** on a `create` or `create_change` step: a retry could file a
  duplicate.
- **`get` is for routines.** A rule-level `get` only records
  `servicenow get … <number>` in the job output; use it as a step and read
  `{{.steps.<id>.json.<field>}}` in a later one.
- **Routines are at-least-once.** A daemon restart during a `create` or
  `create_change` step can file a duplicate.
- **`standard` changes** need the field `standard_change_template_id`.
- **`resolve` is for incidents only** (`table: incident` or none): state 6
  means something else on other tables.
- **No approval by default.** Set `approve: true` on the rule or step to wait
  for a person. The job output is `servicenow <op> <table> <number>`, and
  a later step reads `{{.steps.<id>.json.number}}`.
- **The portal** shows these rules, but its form cannot edit them: use the
  YAML tab.

Ready-made: `siphon template alert-to-servicenow-incident` and
`siphon template servicenow-change-on-deploy`.

### Notes

- **ACLs decide.** A tool call the integration user may not make fails with
  the instance's status and message (for example `403 ... ACL`); the agent
  sees it. Grant the user only the tables the agents need.
- **Where writes can go.** `add_work_note` takes only `incident`,
  `change_request` and `problem`; `cmdb_lookup` only `cmdb_ci` and
  `cmdb_ci_*` tables.
- **Record text is untrusted.** Incident text is written by whoever filed it.
  An agent that reads it should hold only the tools it needs.
- **`close_code` values differ by release** ("Solution provided" or
  "Resolved by caller", and so on). Check *System Definition → Choice Lists*
  for `incident.close_code` on your instance.
- **`javascript:gs.getUserID()`** is what the connection Test sends to find
  the user (`sys_id=javascript:gs.getUserID()` on `sys_user`). If your
  instance blocks `javascript:` in encoded queries, the Test reports an
  error even though the tools work.
- **Redirects and HTML.** A hibernating or SSO-fronted instance answers
  with a login page or a redirect; Siphon reports it as unavailable and
  never follows it.

## Which path when

| You want | Use |
|---|---|
| ServiceNow to trigger rules | webhooks: the Business Rule ([section 4](#4-the-business-rule)) |
| ServiceNow to trigger rules, but it cannot reach Siphon | polling ([Polling instead of webhooks](#polling-instead-of-webhooks)) |
| A deterministic write, with no LLM | the `servicenow:` action ([Writes without an agent](#writes-without-an-agent)) |
| An agent to read or update records | the built-in tools (`package: servicenow`), with no Now Assist licence |
| ServiceNow's own maintained tools | the official MCP server: Siphon logs in to it, but on our demo instance the server itself didn't answer MCP ([below](#the-official-mcp-server)) |

The built-in tools are the supported path for agents.

## The official MCP server

ServiceNow ships its own MCP server (it needs a Now Assist licence). We
probed it on a demo instance on 2026-10-14:

| Check | Result |
|---|---|
| The MCP endpoint | `https://<instance>/sncapps/mcp-server/mcp/<server>` (for example `sn_mcp_server_default`), listed in `sn_mcp_server_registry` |
| RFC 9728 protected-resource metadata | Present at `/.well-known/oauth-protected-resource/<mcp path>`; `authorization_servers` is the MCP URL |
| RFC 8414 authorization-server metadata | Present at `/.well-known/oauth-authorization-server/<mcp path>`: `/oauth_auth.do`, `/oauth_token.do`, PKCE `S256`, authorization code and refresh |
| Issuer | The metadata's `issuer` is the bare instance URL, not the MCP URL the resource metadata names |
| Dynamic client registration | None (`registration_endpoint: null`): a pre-registered OAuth app is required |
| An unauthenticated MCP request | `302` to `/auth_redirect.do`, not `401` with `WWW-Authenticate` |

### Connecting Siphon

Siphon handles both quirks:
- **The redirect counts as a login challenge.** For a source with
  `auth.oauth`, a 3xx from the MCP URL starts the OAuth login. Later, it
  shows as *login required (the server redirected)*.
- **The issuer shape is accepted** when you pin `issuer` to the bare
  instance URL. Siphon then reads the metadata from the MCP path, and only
  if its issuer equals your pin. Without the pin, the login is refused.

1. **In ServiceNow, create an OAuth app:**
   - *System OAuth → Application Registry → New → Create an OAuth API
     endpoint for external clients*;
   - grant type *Authorization Code*;
   - redirect URL `<server.public_url>/oauth/callback`.
2. **Add the source**, with the client secret on stdin:

   ```yaml
   sources:
     servicenow-official:
       type: mcp
       url: https://<instance>.service-now.com/sncapps/mcp-server/mcp/sn_mcp_server_default
       auth:
         oauth:
           client_id: <the app's client ID>
           issuer: https://<instance>.service-now.com   # required: see above
   ```

   ```sh
   siphon apply -f official.yaml --secret sources/servicenow-official.auth.oauth.client_secret=- --dry-run
   siphon apply -f official.yaml --secret sources/servicenow-official.auth.oauth.client_secret=- --yes
   ```

3. **Log in** on the Sources page, or with `siphon connect oauth
   servicenow-official`. Approve the app's consent page as an interactive
   ServiceNow user. A *web service access only* user can't log in to the
   UI, so it can't consent.

### What we found on the demo instance (2026-10-14)

**What worked:**
- **The login completed** through discovery, the consent page and the
  token exchange.
- **The token is valid.** It reads `/api/now/table/...`.

**What didn't:** the MCP server itself.
- **The MCP URL:** `/sncapps/mcp-server/mcp/<server>` answered an
  authenticated MCP `initialize` with an HTML *Page not found* page, for
  every server in `sn_mcp_server_registry`, including the active
  *Quickstart Server*.
- **ServiceNow's own REST tools API:** `/api/sn_mcp_server/mcp_tools_api/tools/server/<name>`
  answered `500 Cannot read property "scope" from undefined`, or *Server
  not found or inactive*.
- **The likely cause:** the instance doesn't have the MCP Server Console
  fully provisioned or licensed (it needs Now Assist).

**Before relying on it,** check that your instance's server answers
`initialize` with JSON. Until then, use the
[built-in tools](#agent-tools).

## Caveats

- **The instance must reach the URL:** a public endpoint, or a MID server.
- **Not Tailscale Funnel.** On the demo instance, deliveries through Funnel
  failed intermittently with `Session contains no certificates - Untrusted`,
  most often in the minutes after Funnel starts. The same deliveries through a
  Cloudflare Tunnel all arrived. Use a Cloudflare Tunnel, or a reverse proxy
  with its own public certificate.
- **A failed delivery is not retried.** It shows only in ServiceNow's
  *Outbound HTTP Requests* log (`sys_outbound_http_log`).
- **The 5-minute window:** a backed-up ECC queue can delay a delivery past
  it, and it is refused. Check *ECC Queue* if events go missing.
- **Event text is untrusted.** A ticket's description is written by whoever
  filed it. Use only envelope fields in command arguments, and give agents
  that read it no tools.
- **Tested on the Australia release; confirm on older ones:** `password2` handling in
  `gs.getProperty`, `changes()` on every element, and that a title with
  non-ASCII text (for example `Café`) is accepted: the body must be sent as
  UTF-8, the encoding it was signed in.
- **Query power.** An agent can send `javascript:` and `NQ` in
  `search_records` queries. The integration user's ACLs and the instance's
  script sandbox are the limit.
