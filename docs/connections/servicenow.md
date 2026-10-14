# ServiceNow

ServiceNow record events (incidents, changes, requests) reach Siphon as a
**signed webhook** sent by a Business Rule. Agent tools and writes are not
part of this release.

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

Siphon's own later writes (not in this release) need a user, and the Business
Rule below must know its name so those writes never echo back. Create it
now: *Web service access only*, with the roles `snc_platform_rest_api_access`
and `itil`. Put its user name in a third property, `siphon.integration_user`
(string). If unset, the script assumes `siphon.integration`.

## 4. The Business Rule

On each table to send (for example `incident`), add a Business Rule: **After**,
on **Insert** and **Update**, with this advanced script:

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
  r.setRequestHeader('Content-Type', 'application/json');
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

The delivery id is `sys_id:sys_mod_count`. A retry of one change carries the
same id, so it collapses onto one replay key; every real update gets a new one.

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

## Caveats

- **The instance must reach the URL:** a public endpoint, or a MID server.
- **The 5-minute window:** a backed-up ECC queue can delay a delivery past
  it, and it is refused. Check *ECC Queue* if events go missing.
- **Event text is untrusted.** A ticket's description is written by whoever
  filed it. Use only envelope fields in command arguments, and give agents
  that read it no tools.
- **To confirm on your release:** `password2` handling in
  `gs.getProperty`, and `changes()` on every element.
