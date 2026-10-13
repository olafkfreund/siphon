---
status: draft
issue: 73
intent: intent/2026-10-13-73-servicenow-inbound.md
---

# Spec: ServiceNow can trigger Siphon rules with signed events

## Design

### How the two sides sign the same way

ServiceNow signs with
`new GlideCertificateEncryption().generateMac(key, "HmacSHA256", msg)`. It
base64-decodes `key` and returns the HMAC in base64. Siphon's
`verifyStandard` (`internal/source/webhook.go:260`) does the same:
- it base64-decodes the secret after `whsec_`;
- it computes HMAC-SHA256 of `webhook-id + "." + webhook-timestamp + "." + body`;
- it compares that with the base64 value after `v1,` in `webhook-signature`.

So the catalog stores `whsec_<hex>`, and the user pastes the bare `<hex>`
into ServiceNow. 64 hex characters are valid base64 and decode to the same
48-byte key on both sides.

`generated` already supports this. `{{generated "secret" "whsec_"}}`
stores the prefixed value, and `Done.HookSecret`, the value shown once, is
the bare one (`internal/catalog/render.go:278-285`). **No Go change.**

### Catalog entry

Add `servicenow` to `internal/catalog/services.yaml`:

```yaml
  - id: servicenow
    name: ServiceNow
    mark: SN
    category: issues
    summary: "ServiceNow record events (incidents, changes, requests) by signed webhook from a Business Rule. Agent tools and writes come in a later release."
    capabilities: [webhooks]
    status: available
    fields:
      - {<<: *f_name, default: servicenow}
    creates:
      - kind: sources
        name: '{{.name}}-hooks'
        when: 'true'
        hook: true
        yaml: |
          type: webhook
          signature: standard-webhooks
          secret: {{generated "secret" "whsec_"}}
    setup: |
      In ServiceNow, add two system properties: siphon.webhook.url (the URL above) and
      siphon.webhook.key (type password2, the secret above). Then add the after Business
      Rule from docs/connections/servicenow.md on each table to send. Full guide:
      docs/connections/servicenow.md.
```

**The source is named `<name>-hooks`.** That's GitHub's pattern, which
keeps the bare `<name>` free for Phase 2's MCP source, so agents can write
`mcp: [servicenow]`. `TestCatalogEntriesRenderAndCommit`
(`internal/web/services_catalog_test.go:46`) checks that `t-<id>` exists.
Its "nothing created" check is extended to also accept `t-<id>-hooks`, for
webhook-only entries with GitHub-style names. That widens the check without
weakening it: some item must still be created. Phase 2 then also creates
`t-servicenow`.

### Setup guide: `docs/connections/servicenow.md`

Register it in `pages` in `docs/internal/gencatalog/main.go:23`, so the
generated connections README links it. The guide covers:

1. **Connect:** `siphon connect servicenow`, which shows the URL and the
   key once.
2. **System properties:** `siphon.webhook.url` (string) and
   `siphon.webhook.key` (`password2`, encrypted at rest).
3. **Integration user** (for Phases 2 and 3, named now so the loop guard
   has it): *Web service access only*, with `snc_platform_rest_api_access`
   and `itil`. Its user name goes in a property, `siphon.integration_user`.
4. **The after Business Rule**, on insert and update, with the advanced
   script:

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

5. **The event shape (intent decision A),** with each field explained.
   `id = sys_id:sys_mod_count`, so retries collapse onto one replay key and
   every real update gets a new one.
6. **Rule examples:**
   - `when: 'event.table == "incident" && event.event == "insert"'`;
   - `on: each`;
   - `id: event.sys_id + ":" + event.sys_mod_count`.
7. **Caveats:**
   - The instance must reach the URL: a public endpoint, or a MID server.
   - A backed-up ECC queue can push a delivery past the 5-minute window,
     and it's refused. Check *ECC Queue* if events go missing.
   - Event text is untrusted.
   - The `password2` and `changes()` behaviour should be confirmed on the
     target release.

### Templates (`docs/templates/`)

- **`servicenow-incident-notify.yaml`** (Notifications & glue):
  - rule `servicenow-new-p1-p2`, source `servicenow-hooks`, firing when
    `event.table == "incident" && event.event == "insert" && event.priority in ["1","2"]`;
  - `on: each`, `id: event.sys_id`;
  - `cmd: [curl, -fsS, -H, "Title: ServiceNow {{.event.number}}", --data-raw, "New P{{.event.priority}} incident: {{.event.short_description}}", https://ntfy.sh/siphon-CHANGE-ME]`.
- **`servicenow-incident-summary.yaml`** (Ops & monitoring): the
  `pagerduty-incident-summary` pattern.
  - A model agent `servicenow-incident-summary` (`approve: false`, no
    tools, read-only), a routine `servicenow-incident-to-phone`, and a rule
    `servicenow-incident-created`.
  - The notify body is `"Summary:\n{{…json.result}}"` (#69).
  - The prompt passes only envelope fields.
- **`cmd/siphon/describe_test.go` `baseConfig`:** add
  `servicenow-hooks: { type: webhook, signature: standard-webhooks, secret: env:AGW_HOOK }`.
- **`docs/templates/README.md`:** a row in each category table.

### Signature test

`TestWebhookServiceNowSigning`, in `internal/source/webhook_modes_test.go`,
next to the Standard Webhooks tests:
- It makes a hex secret `k`, configures `whsec_`+`k`, and signs the way
  `generateMac` does: key = `base64.StdEncoding.DecodeString(k)`, message
  `"<32 hex sys_id>:3.<ts>.<body>"`, base64 output.
- **Accepted:** a correct delivery.
- **Refused:** a changed body, a timestamp 6 minutes old, and the same
  signature keyed with the raw ASCII `k` (the encoding mismatch the guide
  warns about).
- **Replay:** the replay key is the `webhook-id`, so a second delivery
  with the same id is a duplicate.

### Catalog test

In `internal/catalog/catalog_test.go`, render `servicenow` and check:
- the source YAML has `signature: standard-webhooks`, and its secret
  starts with `whsec_`;
- `Done.HookSecret` has no prefix, and is 64 hex characters.

### Docs

- `go generate ./docs`: the connections README, `llms-full.txt` and
  `docs/cli.md`.
- Hand edits: the service list in `docs/README.md`, and the Connections
  line in `llms.txt`.

## Alternatives rejected

- **`signature: token` with a shared header** (the Datadog pattern). It's
  simpler for the admin, but the secret travels with every request and a
  captured delivery can be replayed. Standard Webhooks signs the body and
  the time.
- **A new `servicenow` signature kind.** That's new code for a scheme the
  existing verifier already covers.
- **Flow Designer's REST step.** It needs IntegrationHub, which is
  licensed.
- **An async Business Rule.** `previous` is null there, so `changed` can't
  be computed. The after rule with `executeAsync()` still doesn't block the
  user's save.
- **The webhook source named `<name>`** (the Datadog/PagerDuty pattern).
  That takes the bare name Phase 2's MCP source needs.

## Risks

- **`password2` and `gs.getProperty`:** a script reading a `password2`
  property may get the encrypted value on some releases. The guide flags
  it. If it happens, the fix is a Credential record read through
  `sn_cc.StandardCredentialsProvider`. Check on the owner's instance.
- **`GlideElement.changes()` in after rules:** it's documented for
  before/after rules. Check on the instance.
- **ECC queue delay beyond 5 minutes** means a refused delivery. It's in
  the guide.
- **Extending the catalog test's check** could hide a future entry that
  creates only a `-hooks` source by mistake. That's acceptable: such an
  entry still creates something and still connects.

## Verification

- `go vet ./... && go test -race ./...`: the new signing test, the catalog
  render test, `TestCatalogEntriesRenderAndCommit`,
  `TestTemplatesValidate`, the `docs` template tests and the generated-docs
  freshness tests.
- `siphon template servicenow-incident-notify` prints the template.
- `siphon connect servicenow --help` lists the field.
- **On the owner's instance:**
  - set the properties, add the Business Rule, create a P1 incident: the
    rule fires and the notification arrives;
  - edit the incident as the integration user: no event is sent.
