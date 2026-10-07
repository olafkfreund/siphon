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

`token` is the easiest to set up, but weaker: a captured delivery can be
replayed. Prefer an HMAC signature when the sender supports one.

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
