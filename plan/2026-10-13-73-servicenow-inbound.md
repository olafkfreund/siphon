---
status: approved
issue: 73
spec: spec/2026-10-13-73-servicenow-inbound.md
---

# Plan: ServiceNow can trigger Siphon rules with signed events

## Approved decisions

These are carried over from the spec and the intent.

- **No Go product change.** Reuse `signature: standard-webhooks`. The
  catalog stores `{{generated "secret" "whsec_"}}`, and the user pastes the
  bare 64-hex value (`Done.HookSecret`) into ServiceNow. `generateMac`
  base64-decodes it to the same 48-byte key as `verifyStandard`.
- **Catalog entry `servicenow`:** category `issues`, mark `SN`, field
  `name` (default `servicenow`). It creates one source, `{{.name}}-hooks`
  (`hook: true`), keeping the bare name for Phase 2's MCP source. It lists
  `templates: [servicenow-incident-notify, servicenow-incident-summary]`.
  No API credentials yet.
- **The event envelope (decision A):**
  `{event, table, sys_id, number, sys_mod_count, sys_updated_on, changed, actor, short_description, priority, state}`.
  `webhook-id = sys_id:sys_mod_count`.
- **The ServiceNow side:**
  - an after Business Rule (insert and update) with `executeAsync()`;
  - properties `siphon.webhook.url`, `siphon.webhook.key` (`password2`)
    and `siphon.integration_user`;
  - a loop guard on the integration user;
  - targets Washington DC and later.
- **Rejected:** a token header, a new signature kind, Flow Designer, an
  async Business Rule, and naming the source `<name>`.
- **Event text is untrusted.** Templates use only envelope fields, and the
  agent has no tools.

## Steps

1. **Test first: `internal/source/webhook_modes_test.go`, a new
   `TestWebhookServiceNowSigning`** after the existing tests. Copy the
   shape of `TestWebhookStandard` (`webhook_test.go:180-230`).
   - `k := hex.EncodeToString(32 random bytes)`, with
     `Secret: "whsec_" + k`.
   - `key, _ := base64.StdEncoding.DecodeString(k)`; sign
     `id + "." + ts + "." + body` with HMAC-SHA256 and base64-encode it.
     `id` is `"<32 hex>:3"`.
   - Assert:
     - a correct delivery gets 2xx and the handler sees the id;
     - a changed body is refused (401);
     - a timestamp 6 minutes old is refused;
     - signing with `[]byte(k)` (the raw ASCII) is refused;
     - re-sending the accepted delivery is passed to the handler as a
       duplicate. Mirror `TestWebhookStandard`'s `calls`.
   - **Verify:** `go test ./internal/source -run TestWebhookServiceNowSigning`
     **passes** at once. No product change is needed; this pins the
     compatibility.
   - **Traps:** inject the clock (`Now:`); never use the real time.

2. **`internal/catalog/services.yaml`: the `servicenow` entry.** Place it
   after `jira` in the `issues` group. Use the YAML from the spec's
   "Catalog entry", plus the `templates:` line.
   - The `setup:` text names the two properties and points at
     `docs/connections/servicenow.md`.
   - **Verify:** `go test ./internal/catalog`
     (`TestEmbeddedCatalogueLoads`).
   - **Traps:** if `go generate ./internal/catalog` regenerates anything,
     commit it. With no `nix:`, `catalog-packages.json` shouldn't change;
     confirm.

3. **`internal/catalog/catalog_test.go`, `TestServiceNowEntry`:**
   - `Get("servicenow")` and `Render(map[string]string{"name": "snow"}, Env{})`;
   - there is one item, `sources`/`snow-hooks`, whose YAML contains
     `signature: standard-webhooks` and a `secret:` starting with `whsec_`;
   - `res.Done.HookSecret` matches `^[0-9a-f]{64}$`;
   - `res.Done.Hook == "snow-hooks"`.
   - **Verify:** `go test ./internal/catalog`.
   - **Traps:** check how other tests read the rendered secret.
     `TestSecretAndGeneratedPrefix` (`catalog_test.go:77`) shows where the
     stored value goes. Follow it; don't assume.

4. **`internal/web/services_catalog_test.go:46`,
   `TestCatalogEntriesRenderAndCommit`:** the "nothing created" check
   becomes
   `ce.cur.Load().Sources["t-"+e.ID] == nil && ce.cur.Load().Sources["t-"+e.ID+"-hooks"] == nil && ce.cur.Load().Credentials["t-"+e.ID] == nil`.
   - **Verify:** `go test ./internal/web -run 'TestCatalog'`. Also run the
     whole `./internal/web`, because `TestCatalogEntryTestsAgainstFakeServer`
     loops over entries too.
   - **Traps:** this is the only test change allowed. It widens what counts
     as created, and something must still be created. If another web test
     fails on the new entry, report it; don't edit it.

5. **Templates:**
   - **`docs/templates/servicenow-incident-notify.yaml`** (category
     "Notifications & glue"):
     - header: `# needs: sources/servicenow-hooks (siphon connect servicenow)`;
     - rule `servicenow-new-p1-p2`, source `servicenow-hooks`, `when` as in
       the spec;
     - `on: each`, `id: event.sys_id`;
     - `action.cmd: [curl, -fsS, -H, "Title: ServiceNow {{.event.number}}", --data-raw, "New P{{.event.priority}} incident: {{.event.short_description}}", https://ntfy.sh/siphon-CHANGE-ME]`.
   - **`docs/templates/servicenow-incident-summary.yaml`** (category "Ops &
     monitoring"): copy `pagerduty-incident-summary.yaml`'s structure.
     - Agent `servicenow-incident-summary`: `kind: model`,
       `credential: ollama-local`, `model: llama3.2`, `approve: false`,
       `max_turns: 2`, `timeout: 5m`. Its prompt uses only
       `{{.event.number}}`, `{{.event.short_description}}`,
       `{{.event.priority}}` and `{{.event.state}}`.
     - Routine `servicenow-incident-to-phone`: summary, then notify with
       `"Summary:\n{{.steps.summary.json.result}}"`.
     - Rule `servicenow-incident-created`: incident inserts, `on: each`,
       `id: event.sys_id`, `cooldown: 1m`.
   - **`cmd/siphon/describe_test.go` `baseConfig`:** add
     `servicenow-hooks: { type: webhook, signature: standard-webhooks, secret: env:AGW_HOOK }`
     after `cloudflare-hooks`.
   - **`docs/templates/README.md`:** a row in "Ops & monitoring" and in
     "Notifications & glue", in the existing row format.
   - **Verify:** `go test ./docs ./cmd/siphon`
     (`TestTemplatesParse`, `TestTemplateNamesAreUnique`,
     `TestTemplateArgsStartLiteral`, `TestTemplatesUseDataRaw`,
     `TestTemplatesValidate`).
   - **Traps:**
     - Template item names must be unique across all templates.
     - No argument may start with `{{`.
     - `--data-raw` only.
     - `standard-webhooks` must validate without extra fields in
       `baseConfig`.

6. **`docs/connections/servicenow.md`, the guide from the spec:** connect,
   properties, the integration user, the Business Rule script (verbatim
   from the spec), the envelope table, rule examples, and caveats (MID
   server or a public URL, the ECC queue and the 5-minute window,
   untrusted text, `password2`/`changes()` to confirm on the release).
   - Register it:
     - `docs/internal/gencatalog/main.go:23` `pages`, add
       `"servicenow": "servicenow.md"`;
     - `docs/internal/genllms/main.go:20` `order`, add
       `"connections/servicenow.md"` after `connections/aws.md`.
   - Hand edits:
     - `docs/README.md:37-38`, add "ServiceNow" to the service list, and a
       `[ServiceNow](connections/servicenow.md)` link next to
       GitHub/GitLab;
     - `llms.txt` after the AWS line (~44), add a ServiceNow line in the
       same format.
   - Run `go generate ./docs`.
   - **Verify:** `go test ./docs` (link check, connections README and
     `llms-full.txt` freshness).
   - **Traps:** never hand-edit `docs/connections/README.md`,
     `llms-full.txt` or `docs/cli.md`. The script goes in a fenced
     `javascript` block.

7. **All checks.**
   - Run `go vet ./... && go test -race ./...`.
   - Check `go run ./cmd/siphon template servicenow-incident-notify`.
   - Check `go run ./cmd/siphon connect servicenow --help`.
   - **Verify:** all pass.
   - **Traps:** none.

## Tests

```sh
go test ./internal/source -run TestWebhookServiceNowSigning
go test ./internal/catalog ./internal/web ./docs ./cmd/siphon
go vet ./... && go test -race ./...
```

**On the owner's instance** (before the PR leaves draft):
- create a P1 incident: the rule fires, and the notification arrives;
- update it as the integration user: no delivery.

## Rollback

Revert the PR. It's catalog YAML, docs, templates and tests only: no
schema, store or code-path change. Any `servicenow-hooks` sources users
created stay in their config and keep working, since they're plain
standard-webhooks sources.

## Implementation

Steps 1-6 edit about 12 files, so a `coder` agent writes them and a fresh
Opus agent reviews them. Step 7 and the instance check are mine.
