# Get told when something needs you

**Goal:** a message on your phone or in Slack when:
- an agent run waits for your approval;
- a job fails;
- a polled source keeps failing.

Without this, nothing tells you. Approvals sit until they expire.

## ntfy (your phone)

Install the ntfy app and subscribe to a topic with a long, unguessable name.
The topic name is the secret, so Siphon stores the URL write-only:

```sh
printf %s https://ntfy.sh/siphon-k3v9q2x7p | siphon notify add phone --type ntfy --url - --yes
siphon notify test phone        # → sent (HTTP 200), and your phone buzzes
```

A protected topic on your own ntfy server needs an access token:

```sh
siphon notify add phone --type ntfy --url @ntfy-url.txt --token @ntfy-token.txt --yes
```

## Slack

Create an **incoming webhook** for a channel (Slack: *Apps → Incoming
Webhooks*), then:

```sh
siphon notify add team --type slack --url @slack-hook.txt --events approval,reminder,failed --yes
siphon notify test team
```

## A webhook (anything else)

Siphon POSTs JSON to any https URL. You can add a bearer token
(`--token`).

```sh
siphon notify add relay --type webhook --url @relay-url.txt --token @relay-token.txt --yes
```

```json
{"event": "approval", "rule": "pr-review", "job": 42, "source": "",
 "title": "Approval needed: pr-review (job 42)",
 "message": "agent run waits for approval; expires in 24h",
 "url": "https://siphon.example.com/jobs/42", "at": "2026-10-08T14:02:11Z"}
```

`event` is one of `approval`, `reminder`, `failed`, `source`, `source_ok`
or `test`. `job` is `0`, and `rule` is empty, for source messages.

## What sends a message

| `--events` | When | Sent |
|---|---|---|
| `approval` | a job starts waiting for approval (including each routine step that asks) | once per wait |
| `reminder` | an approval has 4 h or less left (of 24 h) | once per wait |
| `failed` | a job ends `failed`: it errored, was interrupted too often, or its approval expired | once per job |
| `source` | a polled source has failed for 15 minutes; then once more when it recovers | once per outage |

Leave out `--events` to get all four.

**A new channel only hears about new things.** It sends nothing from
before you added it, apart from the last half-minute or so.

## What a message contains

- **Included:** the rule, the job number, the kind of action, the source
  and the time left.
- **Never included:** the event, the prompt, the job's output, or any
  secret. Those can be sensitive, and the message goes to an outside
  service.
- **Link:** set `server.public_url` in `siphon.yaml` and each message links
  to the job in the portal. Without it, the message says which command to
  run (`siphon approve 42`).

No message can approve a job by itself. You approve in the portal or with
the CLI, logged in.

## Verify

- **Portal:** **Notifications** lists each channel with its last delivery,
  a **Test** button, and the recent deliveries. A job's page shows what was
  sent about it.
- **CLI:**
  ```sh
  siphon notify log                  # recent deliveries: sent, pending (retrying), failed, suppressed
  siphon notify log --job 42
  siphon get notify                  # the channels (url and token shown masked)
  siphon delete notify phone
  ```

## Limits

- **Retries:** a message that can't be delivered is retried after 30 s,
  2 min, 10 min, 1 h and 6 h, then marked `failed`, which is recorded in
  the audit log. A 4xx answer (other than 408 or 429) fails at once,
  because a wrong URL or token won't fix itself.
- **At most 30 messages per channel per hour.** The rest are
  `suppressed`, and the next message says how many.
- **Webhook-only sources** are never reported as failing: nothing polls
  them, so Siphon can't tell.
- **Rare duplicates:** if Siphon stops between a successful send and
  recording it, the message is sent again after the restart.
- **Private addresses:** a channel on your LAN (a self-hosted ntfy) must be
  listed in `server.services.private_endpoints`, as `host:port`. Plain
  `http://` is only allowed for such a listed host.
- **`--dry-run` doesn't check the URL itself:** the URL is a secret, so the
  dry run only checks the rest. A URL that isn't allowed (plain `http://` to
  an unlisted host) is refused when you apply.
- **Email:** not built in. Use ntfy's email forwarding, or a webhook that
  sends mail.

## Troubleshooting

- **`notify test` says the url isn't reachable:** the host is private. List
  it in `server.services.private_endpoints`, or use a public URL.
- **`failed` with HTTP 401 or 403:** the token is wrong, or the topic is
  protected. Add the channel again with the right `--token`.
- **`failed` with HTTP 404:** the Slack hook was revoked, or the URL is
  wrong. Add the channel again.
- **Nothing for an approval:** check that the channel's events include
  `approval`, and that the approval started after you added the channel.
