---
status: approved
issue: 83
author: olafkfreund
---

# Intent: Azure Event Grid events trigger rules

## Problem

Azure Event Grid is "Not yet" in the catalog. Azure services publish their
events through it: a blob created, a resource group changed, a Key Vault
secret near expiry, a container image pushed, a VM deallocated. Siphon
can't receive them today, for three reasons:

1. **The subscription handshake.** Event Grid won't deliver to a webhook
   until the endpoint proves it wants the events.
   - **Event Grid schema:** the endpoint must answer a
     `Microsoft.EventGrid.SubscriptionValidationEvent` with
     `{"validationResponse": "<code>"}`.
   - **CloudEvents 1.0 schema:** the endpoint must answer an `OPTIONS`
     request carrying `WebHook-Request-Origin` with
     `WebHook-Allowed-Origin`.
   - Siphon answers every verified delivery with `202` and no body, and it
     doesn't route `OPTIONS` at all.
2. **Batches.** Event Grid posts a JSON *array* of events. Siphon turns one
   request into one event, so a rule would see the whole array.
3. **Authentication.** Event Grid doesn't sign deliveries. It can add
   static custom headers (delivery properties, which can be marked
   secret), or use a Microsoft Entra bearer token. Siphon's `token` mode
   already covers the first.

## Proposed outcome

- **A webhook option for Event Grid sources:**
  - it answers both handshakes, Event Grid schema and CloudEvents;
  - it splits a batch into one event per element. Each element's `id` is
    the replay key, so Event Grid's retries are duplicates.
- **Authentication is the existing `token` mode,** with a secret custom
  header that the user sets on the Event Grid subscription.
- **An `azure-eventgrid` catalog entry,** with setup text for the
  subscription: endpoint URL, the delivery property header, and the event
  schema.
- **A template:** a Key Vault "secret near expiry" event sends a
  notification.

## Affected users and systems

- Azure users.
- `internal/source/webhook.go` (handshakes, batch splitting, `OPTIONS`),
  `internal/config` (the option and its validation), the HTTP routing for
  `/hook/` (`OPTIONS`), the catalog, docs and templates.

## Constraints

- **The handshake can't trigger a rule,** and it can't be used to make
  Siphon call out. The `validationUrl` (manual validation) is never
  fetched by Siphon.
- **Answering a handshake only happens on a source configured for Event
  Grid.** Other webhook sources stay exactly as they are.
- **A batch has a size limit** (Event Grid allows up to 1 MB per batch).
  The existing body limit and the per-source rate limit apply to every
  event in the batch.
- **No Entra token validation in this issue.** That needs JWKS fetching,
  shared with Google Pub/Sub (#88). It's noted as a follow-up.

## Open questions

- **Is the handshake authenticated?** Microsoft's docs suggest custom
  delivery headers are sent on the validation request too. If they are, the
  handshake answer is only given after the token check. If they aren't, it
  has to be answered unauthenticated, which echoes a code and starts
  nothing.
  - **I'd require the token,** and fall back to unauthenticated *only* if
    a test against real Event Grid shows the header is missing. Do you have
    an Azure subscription to test on? It's Event Grid's free tier, with a
    system topic on a storage account.
- **Both schemas, or only CloudEvents?** **I'd support both.** The Event
  Grid schema is still the default for system topics.
- **How should it be configured:** a new signature mode, or a
  `format: eventgrid` option on top of `token`? **I'd use the option.**
  Authentication and envelope stay separate, and a later Entra mode can
  combine with it.

## Decisions (owner, at approval)

- **The handshake requires the token** (the delivery-property header). An
  unauthenticated fallback only comes if real Event Grid turns out not to
  send it on validation.
- **Both schemas:** Event Grid and CloudEvents 1.0.
- **Configured as `format: eventgrid`** on a `token`-mode webhook source.
- **No Azure subscription was named.** It's tested against Microsoft's
  documented payloads, and a real Event Grid check is left as a follow-up.
