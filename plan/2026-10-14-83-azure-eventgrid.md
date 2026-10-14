---
status: approved
issue: 83
spec: spec/2026-10-14-83-azure-eventgrid.md
---

# Plan: Azure Event Grid events trigger rules

## Approved decisions

- **Configuration:** `format: eventgrid` on a webhook source, only with
  `signature: token`. A delivery `id` header isn't allowed with it, and any
  other format value is an error.
- **Routing:** `OPTIONS /hook/{source}` is routed. The handler serves it
  only for `eventgrid` sources, and others get 405.
- **The token check runs first** for `OPTIONS` and `POST`; a failure gets
  401.
- **`OPTIONS`:** with a plain-hostname `WebHook-Request-Origin`, answer
  `200` with `WebHook-Allowed-Origin: <origin>` and
  `WebHook-Allowed-Rate: *`. Otherwise answer 400. Nothing is delivered.
- **`POST`:** the body is a JSON array (the batch) or an object (one
  CloudEvent); anything else gets 400.
  - **The validation handshake:** exactly one element with `eventType` =
    `Microsoft.EventGrid.SubscriptionValidationEvent` gets
    `200 {"validationResponse": data.validationCode}` (a string of at most
    128 characters). `validationUrl` is never fetched.
  - **Delivery:** every element needs a non-empty string `id`, or the
    request gets 400. Per element, run `Limit.Allow`, getting 429 when it
    fails, then `deliver` keyed by the `id`; duplicates are skipped.
  - **The response:** `200` when everything is delivered or a duplicate,
    and `500` if any delivery fails.
- **The catalog's `azure-eventgrid` entry** uses `token`,
  `token_header: X-Siphon-Key`, a generated secret and `format: eventgrid`.
  The setup text covers the delivery property and mentions
  `limits.http_max_body`.
- **The template, `azure-keyvault-secret-expiry`:** a rule on
  `(event.eventType ?? event.type) == "Microsoft.KeyVault.SecretNearExpiry"`
  sends an ntfy notification.
- **Entra JWT, query secrets and manual validation** are out of scope. A
  real-Azure check is a follow-up.

## Steps

0. **Base.** Wait for PR #94 (Mattermost) to merge, then
   `git rebase origin/main`. #94 adds `decodeBody` and changes the handler
   this plan edits. Use `decodeBody` for the JSON decode.

1. **Config** (`internal/config/config.go`, `explain.go`, the schema).
   - Add `Format string \`yaml:"format"\`` to `Source`, with the
     validation per the decisions, next to the signature switch.
   - **Tests:** a table covering a valid eventgrid source, eventgrid with
     sha256, eventgrid with `id`, and an unknown format.
   - **Verify:** `UPDATE_SCHEMA=1 go test ./internal/config` once, then
     `go test ./internal/config ./cmd/siphon`. `TestExplainCommand` may
     need the new field. If so, update its expected text and log it.

2. **Handler and routing** (`internal/source/webhook.go`,
   `internal/job/hooks.go`, `internal/web/web.go:171`).
   - **`WebhookOptions`** gains `Format`, passed from `hooks.go`.
   - **`web.go`:** register `OPTIONS /hook/{source}` with the same handler
     function as `POST`.
   - **`webhook.go:91`:** allow `OPTIONS` only when `o.Format ==
     "eventgrid"`. After the existing `token` check, branch into a new
     function, `serveEventGrid(w, r, o, data, headers, deliver)`, that does
     `OPTIONS`, the validation event, and batch delivery per the
     decisions. Keep the token check where it is, so both paths share it.
   - **`testdata/eventgrid/`:** Microsoft's documented examples for the
     validation event, a two-event Event Grid batch
     (`Microsoft.Storage.BlobCreated`), a CloudEvents single event, and a
     CloudEvents batch.
   - **Tests** in `webhook_eventgrid_test.go`. Cover:
     - validation with the token gets 200 and the code, and nothing is
       delivered;
     - validation without the token gets 401;
     - `OPTIONS` with the token gets 200 with the header, and 401 without
       it;
     - `OPTIONS` with a bad origin gets 400;
     - `OPTIONS` on a non-eventgrid source gets 405;
     - a batch of 2 is delivered as 2 events keyed by `id`;
     - re-sent, it gives 2 duplicates and still 200;
     - a single CloudEvent is delivered as 1 event;
     - an element without `id` gets 400 and nothing is delivered;
     - an injected `deliver` error gets 500;
     - a limiter at 1 gets 429 on the second element.
   - **Verify:** `go test -race ./internal/source ./internal/job ./internal/web`.
   - **Traps:**
     - Never echo anything but `validationCode` and the checked origin.
     - The headers map must still drop the token header.
     - Unix socket paths don't apply here.

3. **Catalog, template and docs.**
   - **`services.yaml`:** replace the `not-yet` line (`:797`) with the
     entry. Add a `TestAzureEventGridEntry` render test.
   - **`docs/templates/azure-keyvault-secret-expiry.yaml`,** plus a
     `baseConfig` line,
     `azure-eventgrid-hooks: { type: webhook, signature: token, token_header: X-Siphon-Key, secret: env:AGW_HOOK, format: eventgrid }`,
     a README row, and the entry's `templates:`.
   - **Docs:** the webhook docs (grep for where signature modes are listed)
     get a line on `format: eventgrid`, and `docs/llm.md` gets one line.
     Then run `go generate ./docs`.
   - **Verify:** `go test ./internal/catalog ./internal/web ./docs ./cmd/siphon`.
   - **Traps:** no template argument starts with `{{`, `--data-raw` only,
     and no hand-edited generated files.

4. **All checks, and curl (mine).**
   - `go vet ./... && go test -race ./...`.
   - Run a throwaway Siphon and curl:
     - the validation event;
     - `OPTIONS` with `WebHook-Request-Origin: eventgrid.azure.net`;
     - a two-event batch, which fires the rule twice;
     - the same batch again, which gives duplicates.

## Tests

```sh
go test -race ./internal/source ./internal/job ./internal/web ./internal/config
go test ./internal/catalog ./docs ./cmd/siphon
go vet ./... && go test -race ./...
```

## Rollback

Revert the PR. Sources with `format: eventgrid` then fail validation.
Nothing else changes.

## Implementation

Steps 1–3 edit about 12 files, so a `coder` agent does them and a fresh
Opus agent reviews them. Steps 0 and 4 are mine.
