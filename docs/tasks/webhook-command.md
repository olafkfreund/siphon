# Run a command when a webhook arrives

**Goal:** a service (a deploy tool, a script, Home Assistant, a CI job)
POSTs to Siphon, and Siphon runs a command in a sandbox.

## CLI

The fastest way creates the webhook, its secret and the rule in one go:

```sh
siphon new task --name deploy-done --webhook deploy-hook \
  --when 'event.status == "success"' --on each --id event.id \
  --cmd '["echo", "deploy {{.event.id}} finished"]' --yes
```

Siphon prints the URL, the header (`X-Siphon-Key`) and the secret, **once**.
Give them to the sender:

```sh
curl -H "X-Siphon-Key: <secret>" -d '{"id":"r42","status":"success"}' https://siphon.example.com/hook/deploy-hook
```

**Prefer HMAC signatures** (the sender signs the body)? Start from the template:

```sh
siphon template webhook-command > deploy.yaml      # signature: sha256 in X-Signature
siphon apply -f deploy.yaml --secret sources/deploy-hook.secret=@hook.key
```

## Choosing the signature

| `signature:` | The sender… | Use for |
|---|---|---|
| `github` | signs with HMAC in `X-Hub-Signature-256` | GitHub |
| `sha256` | signs with HMAC-SHA256 (hex) in a header you name | most custom senders |
| `standard-webhooks` | signs per the Standard Webhooks spec (`whsec_…`) | Svix, Clerk, Resend, … |
| `token` | sends the secret itself in a header you name | GitLab, EventBridge, Alertmanager, Home Assistant, scripts |
| `body-token` | puts the secret in the request body (JSON or form field `token`, or `token_field:`); Siphon removes it from the event, and `reply_text:` answers 200 `{"text": …}` | Mattermost outgoing webhooks and slash commands |
| `token` + `format: matrix` | `token_header: Authorization`, the secret is the homeserver's `hs_token` sent as `Bearer <secret>`; answers `PUT /hook/<name>/_matrix/app/v1/transactions/<id>` with `{}` and delivers each event keyed by `event_id`, answers ping, declines user and room queries; `allow_query_token: true` also accepts `?access_token=` (it puts the `hs_token` in the URL, so reverse-proxy logs will record it). A missing token gets 401 `M_UNAUTHORIZED`, a wrong one 403 `M_FORBIDDEN`. Unencrypted rooms only | Matrix application services |
| `ed25519` | verifies `X-Signature-Ed25519` over `X-Signature-Timestamp` + body with a hex public key as the secret (5-minute window) | Discord (a public key is not a secret) |
| `ed25519` + `format: discord` | as `ed25519`, and answers Discord's PING, ignores autocomplete, and delivers each interaction keyed by its `id` with a deferred reply (or `reply_text:` as the message) | Discord slash commands and components |
| `token` + `format: eventgrid` | as `token`, and answers Azure Event Grid's `OPTIONS` and validation handshakes and delivers each event of a batch (one rate-limit charge per request; keyed by its `id`; `validationUrl` is never fetched) | Azure Event Grid (put the secret in a delivery property) |
| `sha256` + `format: notion` | as `sha256` with `X-Notion-Signature` and prefix `sha256=`. With no `secret:` the source only accepts Notion's one-time `{"verification_token": …}` POST and delivers nothing; an operator accepts the token on the Sources page (or `POST /api/sources/<name>/notion-verification` with `{"token": …}`, the token that was shown), which stores it as the secret. Order: create the subscription in Notion, copy the token from the Sources page, paste it into Notion and Verify, then press "Use this token". A wrong token that was accepted: delete `secret:` from the source's YAML and use "Resend token" in Notion; a restart loses an unaccepted token, so resend it too | Notion |
| `slack` | signs `v0:<timestamp>:<body>` (HMAC-SHA256, `X-Slack-Signature`, ±5 min); Siphon also answers Slack's signed `url_verification` with its challenge | Slack (paste the app's Signing Secret) |
| `stripe` | signs `<t>.<body>` in `Stripe-Signature` (`t=…,v1=…`; any `v1` may match, so rotation works; ±5 min) | Stripe (paste the endpoint's signing secret) |

`token` is the easiest to set up, but weaker: a captured delivery can be
replayed. Prefer an HMAC signature when the sender supports one.

**`sha256` options** for providers that vary the format:
- `signature_prefix: v1=`: the header may hold a comma-separated list of
  `v1=<hex>` signatures, and any one may match (PagerDuty, during secret
  rotation).
- `timestamp_header` plus `timestamp_separator: ":"`: the signed text is
  `<timestamp>:<body>` instead of the default `<timestamp>.<body>`
  (Grafana).

For a known service, `siphon connect <service>` sets all of this for you
(see [all services](../connections/README.md)).

## Commands, safely

- **`cmd:` is an argv list, never a shell.** No pipes, no `&&`, no `$VAR`.
  Need a pipeline? Write a script and run the script.
- `{{.event.field}}` is filled in per argument, and a filled-in value may
  not start with `-`.
- The command runs in a systemd sandbox (NixOS module or microVM), with no
  access to Siphon's state or other runs.

## Verify

- **Portal:** **Rules** shows the flow, **Sources** shows the last
  delivery, and **Jobs** shows each run and its output.
- **CLI:** `siphon test deploy-done --last` shows what would run;
  `siphon jobs` lists the runs; `siphon why deploy-done` explains a missing
  run.

## More templates

`webhook-to-ntfy` (forward anything to your phone), `service-failed`
(systemd `OnFailure=`), `homeassistant-event`, `standard-webhooks`.
