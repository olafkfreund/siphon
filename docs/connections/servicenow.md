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

## Caveats

- **The instance must reach the URL:** a public endpoint, or a MID server.
- **The 5-minute window:** a backed-up ECC queue can delay a delivery past
  it, and it is refused. Check *ECC Queue* if events go missing.
- **Event text is untrusted.** A ticket's description is written by whoever
  filed it. Use only envelope fields in command arguments, and give agents
  that read it no tools.
- **To confirm on your release:** `password2` handling in
  `gs.getProperty`, `changes()` on every element, and that a title with
  non-ASCII text (for example `Café`) is accepted: the body must be sent as
  UTF-8, the encoding it was signed in.
- **Query power.** An agent can send `javascript:` and `NQ` in
  `search_records` queries. The integration user's ACLs and the instance's
  script sandbox are the limit.
