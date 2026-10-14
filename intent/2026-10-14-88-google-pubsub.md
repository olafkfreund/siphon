---
status: draft
issue: 88
author: olafkfreund
---

# Intent: Google Cloud Pub/Sub push subscriptions trigger rules

## Problem

Google Pub/Sub is "Not yet" in the catalog. It's how Google Cloud services
emit events: Cloud Build results, GCS object changes, Cloud Monitoring
alerts, Artifact Registry pushes, budget alerts and audit-log sinks. A
**push subscription** delivers each message as an HTTPS POST. Siphon can't
take them today:

1. **Authentication is a Google-signed OIDC ID token, not an HMAC.**
   - With authentication on, Pub/Sub sends
     `Authorization: Bearer <JWT>`, signed RS256 by Google.
   - The token's claims are: `iss` is `https://accounts.google.com`, `aud`
     is the configured audience, and `email` is the push service account.
   - Verifying it means fetching and caching Google's public keys (JWKS),
     then checking the signature, expiry, issuer, audience and email.
   - Siphon has no such mode for webhooks.
2. **The payload is an envelope.** It's
   `{"message":{"data":"<base64>","attributes":{…},"messageId":"…","publishTime":"…"},"subscription":"…"}`.
   The useful content is base64 inside `data`. Rules would need the
   decoded data.
3. **Replays.** Pub/Sub redelivers until it gets a 2xx. The `messageId` is
   the natural replay key.

## Proposed outcome

- **A webhook signature mode, `google-oidc`.** It verifies the bearer ID
  token:
  - **Signature and expiry** are checked against Google's JWKS, fetched
    through the guarded egress client and cached. There's no new
    dependency: `coreos/go-oidc` is already used for portal sign-in.
  - **`iss`** must be Google.
  - **`aud`** must equal the source's configured audience.
  - **`email`** must equal the configured service account, and
    `email_verified` must be true.
  - **Any failure** gets 401.
- **A `pubsub` format on that mode:**
  - it unwraps the envelope: the event is `{data, attributes, message_id,
    publish_time, subscription}`, where `data` is decoded from base64, and
    parsed as JSON when it is JSON;
  - the `messageId` is the replay key;
  - it answers `204`, so a duplicate is acknowledged too.
- **A `google-pubsub` catalog entry:** the audience, which defaults to the
  hook URL, the push service account email, and setup text for creating
  the push subscription.
- **A template:** a failed Cloud Build sends a notification.
- **Reuse:** the same token verifier is the base for Azure Event Grid's
  Entra-auth option later. That's a follow-up, not part of this issue.

## Affected users and systems

- Google Cloud users.
- `internal/source/webhook.go` (the mode and the format), a small OIDC
  verifier helper in `internal/source`, `internal/config`, `internal/job`
  (wiring the guarded HTTP client), the catalog, docs and templates.

## Constraints

- **The JWKS fetch** goes through the existing guarded client, which
  refuses private hosts and redirects. Keys are cached, and refreshed when
  a token names an unknown key id. Requests never block for long on a slow
  key server.
- **The bearer token is never logged,** and it's dropped from event
  headers.
- **No new dependency.**
- **Message content is untrusted.**

## Open questions

- **Is the service account email required?** **I'd require it.** Any
  Google-signed token with the right audience would otherwise pass,
  including one minted by an unrelated project's service account.
- **The audience default:** Pub/Sub uses the push endpoint URL when no
  audience is set. **I'd default to the hook URL** (`server.public_url` +
  `/hook/<name>`), and allow a custom one.
- **Should JSON `data` be decoded into fields?** **I'd decode JSON into
  fields** (`event.data.status`), and keep it as a string when it isn't
  JSON. Most Google emitters (Cloud Build, GCS) send JSON.
