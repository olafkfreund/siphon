# Getting started

In about 10 minutes you'll have Siphon running, your CLI logged in, a model
connected, and two working tasks: one that runs a command and one that has an
AI agent summarise an event. Each step shows the CLI, then where to see the
result in the portal.

You need Siphon running somewhere. Pick one way from [Ways to run Siphon](../README.md#ways-to-run-siphon):

| You have | Run |
|---|---|
| NixOS | `services.siphon.enable = true;` (full sandbox; the `siphon` CLI is on your PATH) |
| Nix + KVM, not NixOS | `nix run github:olafkfreund/siphon#microvm` (full sandbox in a VM) |
| Docker or Podman | the `ghcr.io/olafkfreund/siphon` image (no sandbox: for trying it out) |

The portal and API listen on the address in your config, for example
`http://127.0.0.1:8080`, and the portal token is the one in `server.token`.

## 1. Log the CLI in

```sh
siphon login http://127.0.0.1:8080        # paste the portal token at the prompt (it isn't echoed)
siphon status
```

```text
logged in to http://127.0.0.1:8080 (saved to ~/.config/siphon/client.yaml)
sources: 0
rules: 0
approvals waiting: 0
jobs (last 100): none
```

The CLI talks to the running Siphon over its API, from this machine or
another one. The URL and token are saved together in a 0600 file you own,
and always used as a pair, so the token never goes to another address.

- **In scripts with no saved login,** set `SIPHON_URL` and
  `SIPHON_TOKEN_FILE`.
- **To reach a different server,** pass `-url` with its own `-token-file`.
- **On NixOS,** `SIPHON_URL` is already set, so `siphon login` alone is
  enough.
- **Plain `http://`** is only accepted for this machine (loopback). For a
  remote Siphon use `https://`, or pass `--insecure-http` if you really
  mean it.

**In the portal:** open the address in a browser, sign in with the same
token, and see the empty **Dashboard**.

## 2. Connect a model

Agents need a connection. A local [Ollama](https://ollama.com) is free and
private:

```sh
siphon connect model ollama --name ollama-local --url http://127.0.0.1:11434
```

```text
connected model endpoint "ollama-local"
test: ok, 14 models: gemma4:12b, gemma4:26b, qwen2.5-coder:14b, qwen2.5:7b, …
```

> An Ollama on this host or your LAN is a *private* address, so the operator
> lists it once in `siphon.yaml`:
> `server: { models: { private_endpoints: ["127.0.0.1:11434"] } }`.
> The NixOS module does this automatically for a local `services.ollama`.

Prefer a subscription? Use `siphon connect login claude` (or `codex`, `agy`).
See [Connections](connections/logins.md).

**In the portal:** **Connections** lists `ollama-local` with its models.

## 3. Your first task: a webhook that runs a command

```sh
siphon new task --name hello --webhook hello-hook \
  --when 'event.msg != ""' --on each --id event.msg \
  --cmd '["echo", "got {{.event.msg}}"]' --yes
```

```text
applied revision 2 (2 items)
webhook source "hello-hook"
  URL:    http://127.0.0.1:8080/hook/hello-hook
  header: X-Siphon-Key: <the secret>
  secret: e7534f00…                (shown once, copy it now)
```

That created two items: a webhook **source** and a **rule** that runs `echo`
for each new message. Run `siphon new task` with no flags for a wizard
that asks the same questions.

Send it an event, as any service would:

```sh
curl -H "X-Siphon-Key: <the secret>" -d '{"msg":"hi from curl"}' \
  http://127.0.0.1:8080/hook/hello-hook
siphon jobs
```

```text
ID  RULE   STATE  ATTEMPT  CREATED              EXIT
1   hello  done   0        2026-10-07 15:32:06  0
```

Check what the rule does with the last real event, without running anything:

```sh
siphon test hello --last
```

```text
KEY           ACTION   WOULD RUN              ERROR
hi from curl  command  echo got hi from curl
```

**In the portal:** **Rules** shows `hello` as a flow (source → condition →
action). **Jobs** shows job 1 and its output.

## 4. A task with an AI agent

Start from a template instead of writing YAML yourself:

```sh
siphon template                                   # the list, by category
siphon template summarise-webhook > digest.yaml   # webhook -> local model summary
```

Open `digest.yaml`, set `model:` to one your connection listed (for example
`qwen2.5:7b`), then check and apply it. Its header says which secret to pass:

```sh
head -c 32 /dev/urandom | base64 > digest.key
siphon apply -f digest.yaml --secret sources/digest.secret=@digest.key --dry-run   # see the diff
siphon apply -f digest.yaml --secret sources/digest.secret=@digest.key --yes
```

Fire it, then approve the run. Agents wait for a human by default:

```sh
curl -H "X-Siphon-Key: $(cat digest.key)" \
  -d '{"service":"api","deploys":3,"errors_24h":2,"p95_ms":180,"day":"wed"}' \
  http://127.0.0.1:8080/hook/digest
siphon get approvals
siphon approve 3
siphon jobs show 3
```

```text
state:  done
output:
{"result":"- On Wednesday, 3 deploys were conducted for the API service.\n- There were 2 errors reported within the last 24 hours.\n- The 95th percentile response time for API service requests is 180 milliseconds.","turns":1,…}
```

**In the portal:** the run waits in **Approvals** (approve it there instead,
if you like), then **Jobs** shows the summary.

**Get told when a run waits:** nothing tells you an approval is waiting
until you add a channel. One line sets up your phone (the app is ntfy):

```sh
printf %s https://ntfy.sh/<a-long-random-topic> | siphon notify add phone --type ntfy --url - --yes
siphon notify test phone
```

More in [Notifications](tasks/notifications.md).

## 5. When something doesn't happen

```sh
siphon why digest
```

```text
rule digest
 ✓ enabled
 ✓ source digest (webhook) is idle
 ✓ last event at 2026-10-07 15:41:03
 ✓ the last event satisfies the condition
 ✗ the last event was held back by the cooldown
 ✓ last fired 2026-10-07 15:40:58
 ✓ no evaluation errors
 ✓ no webhook deliveries refused
 ✗ approvals: a job is waiting

Likely reason: The last event arrived 5s after the rule fired, inside its 30s cooldown, so it was held back.
Next: siphon get audit --rule digest
```

Here two events arrived 5 seconds apart. The first fired the rule (and
waits for approval); the second came inside the 30-second `cooldown` and was
held back, on purpose. `why` checks every reason a rule might not fire, and
ends with the likely one and the next command to run. More in [Troubleshooting](troubleshooting.md).

## Where next

- **Get told:** [Notifications](tasks/notifications.md) for approvals,
  failed jobs and failing sources, on ntfy, Slack or a webhook.
- **Run it for real:** [Backup and monitoring](tasks/backup-and-monitoring.md):
  backups, restore, Prometheus metrics and how long history is kept.
- **Ready-made tasks:** [the templates](templates/README.md): code review,
  CI failures, alerts, uptime, AWS, Home Assistant, nightly reports.
- **How-tos by task:** [the guide index](README.md).
- **Connecting services:** [GitHub](connections/github.md), [GitLab](connections/gitlab.md),
  [AWS](connections/aws.md), [models](connections/models.md), [logins](connections/logins.md).
- **Let an AI do it:** [Drafting tasks in plain language](drafting.md),
  and [Siphon for AI assistants](llm.md).
- **Every command:** [CLI reference](cli.md).
