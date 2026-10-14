---
status: approved
issue: 88
spec: spec/2026-10-14-88-google-pubsub.md
---

# Plan: Google Cloud Pub/Sub push subscriptions trigger rules

## Approved decisions

- **`signature: google-oidc`:**
  - `service_account` is required and email-shaped; `audience` is
    optional;
  - with no audience, `server.public_url` must be set, and the default is
    `public_url` + `/hook/<name>`;
  - no secret is required;
  - `secret`, `signature_header`, `token_header`, `timestamp_header`,
    `token_field` and `id` are refused;
  - `audience` and `service_account` are refused on other modes.
- **`format: pubsub`** needs `google-oidc`.
- **The verifier** (`internal/source/googleoidc.go`) uses go-oidc's
  `NewRemoteKeySet` on Google's certs URL, through the guarded client
  (`source.HTTPClient(false, 10s, 1 MiB)`).
  - The verifier skips go-oidc's own issuer and client-ID checks.
  - Siphon checks by hand: `iss` is `https://accounts.google.com` or
    `accounts.google.com`; `aud` contains the audience; `email_verified`
    is true.
  - It returns `email`. There's one verifier per daemon.
- **The handler, `google-oidc`:**
  - the `Authorization: Bearer` token is verified within a 5-second
    context;
  - the email must equal `service_account` (case-insensitive);
  - any failure gets 401 "token invalid";
  - the header is dropped from the event.
- **`format: pubsub`:**
  - it unwraps the envelope (no `message` or `messageId` gives 400);
  - it decodes the base64 `data` (an error gives 400), parsed as JSON when
    valid, otherwise a string;
  - the event is `{data, attributes, message_id, publish_time,
    subscription}`;
  - it runs `Limit.Allow` once (429), then `deliver` keyed by `messageId`
    (500 on error), and answers 204, a duplicate included.
- **`google-oidc` without the format** answers 202 with the raw body and a
  body-hash key.
- **The catalog's `google-pubsub`** and the
  `gcp-cloud-build-failed` template.

## Steps

1. **Config** (`internal/config/config.go`, `explain.go`, the schema).
   - **The `Audience` and `ServiceAccount` fields.**
   - **The `google-oidc` case** in the signature switch, the `pubsub` case
     in the format switch, the required-secret skip, and the refusals.
   - **Tests:**
     - a valid source with an audience;
     - a valid source with no audience but with `public_url`;
     - no audience and no `public_url`;
     - a missing `service_account`;
     - a bad email;
     - `google-oidc` with a secret;
     - `pubsub` with `sha256`;
     - `service_account` on `sha256`.
   - **Verify:** `UPDATE_SCHEMA=1 go test ./internal/config` once, then
     `go test ./internal/config ./cmd/siphon`.

2. **The verifier and handler** (`internal/source/googleoidc.go`,
   `webhook.go`, `internal/job/hooks.go` and `pipeline.go`).
   - **`GoogleVerifier`,** per the decisions, plus an unexported
     `newGoogleVerifier(hc, certsURL)` for tests.
   - **`WebhookOptions`** gains `Audience`, `ServiceAccount` and `Google`.
     `hooks.go` passes them, computing the default audience from
     `cfg.Server.PublicURL`. The Pipeline holds one lazily created
     verifier.
   - **The `google-oidc` case,** plus `servePubSub`, entered like the
     other formats. Pub/Sub sends one message per request, so the single
     `Limit.Allow` before it is fine.
   - **Tests** in `webhook_pubsub_test.go`:
     - a test RSA key and an `httptest` JWKS;
     - tokens signed with go-jose, which is already a dependency;
     - every case in the spec's verification list;
     - the bearer header absent from the event headers.
   - **Verify:** `go test -race ./internal/source ./internal/job ./internal/web`.
   - **Traps:**
     - Inject the clock (`Now`) for expiry.
     - Never log the token.
     - The JWKS server is loopback, so the test verifier uses a plain
       client. Production uses the guarded one.

3. **Catalog, template and docs.**
   - **`services.yaml`:** replace the `google-pubsub` not-yet line
     (`:930`) per the spec, and add `TestGooglePubSubEntry`.
   - **`docs/templates/gcp-cloud-build-failed.yaml`,** plus:
     - a `baseConfig` line,
       `google-pubsub-hooks: { type: webhook, signature: google-oidc, format: pubsub, service_account: push@p.iam.gserviceaccount.com, audience: https://siphon.example.com/hook/google-pubsub-hooks }`;
     - a README row and the entry's `templates:`;
     - the webhook docs rows and `docs/llm.md`.

     Then run `go generate ./docs`.
   - **Verify:** `go test ./internal/catalog ./internal/web ./docs ./cmd/siphon`.
   - **Traps:**
     - The catalog commit test must pass for a source with no secret.
     - No argument starts with `{{`.

4. **All checks, and a curl test (mine).**
   - `go vet ./... && go test -race ./...`.
   - A scratchpad program serves a JWKS on loopback and signs tokens. A
     throwaway Siphon built with a test hook to point at that JWKS isn't
     possible without code changes, so this step relies on the unit tests,
     plus a negative curl: no bearer gets 401.

## Tests

```sh
go test -race ./internal/config ./internal/source ./internal/job ./internal/web
go test ./internal/catalog ./docs ./cmd/siphon
go vet ./... && go test -race ./...
```

## Rollback

Revert the PR. Sources with `google-oidc` or `pubsub` then fail
validation.

## Implementation

Steps 1–3 edit about 14 files, so a `coder` agent does them and a fresh
Opus agent reviews them. Step 4 is mine.

## Deviations

- **Step 1:** `TestExplainCommand`'s expected signature list gains
  `google-oidc`. Matrix (#99) wasn't on main yet, so its format case is
  merged at rebase.
