# Troubleshooting

## Start with `why`

```sh
siphon why <rule>
```

It checks, in order:
- whether the rule is enabled;
- the source's health and last error;
- when the last event came;
- whether that event matches;
- cooldown and edge state;
- evaluation errors;
- refused webhook deliveries;
- waiting approvals.

It ends with the **likely reason** and the **next command**. `-o json` gives
the same as data.

| Reason | What it means | Fix |
|---|---|---|
| `disabled` | someone ran `siphon disable` | `siphon enable <rule>` |
| `no_events_yet` | the source never delivered | check the sender's URL and secret; `siphon get sources` |
| `source_error` | polling failed (DNS, TLS, HTTP status, bad JSON) | the error is shown; fix the URL or allow the host |
| `webhook_rejected` | deliveries were refused (signature, timestamp, rate limit) | check the secret and the header name the sender uses |
| `eval_error` | `when:` or `id:` failed on a real event (a missing field, a wrong type) | `siphon test <rule> --last` shows the error; guard with `event.x != nil` |
| `condition_false` | the last event didn't match | `siphon test <rule> --last`; compare with `siphon get sources` / the event |
| `edge_already_true` | `on: edge` already fired and the condition is still true | expected; add `repeat:` to re-fire while true |
| `cooldown` | the event came inside the rule's cooldown | expected; shorten `cooldown:` if needed |
| `fired` | it did fire, maybe as a duplicate (`on: each` fires once per `id` for 7 days) | `siphon get audit --rule <rule>`; make `id:` unique per event |
| `awaiting_approval` | it fired and is waiting for you | `siphon get approvals` |

## Test without waiting for an event

```sh
siphon test <rule> --last                 # against the last real event
siphon test <rule> event.json             # against a sample
siphon test <rule> - --header X-GitHub-Event=pull_request < event.json
```

## Common errors

- **`422` / exit 3 on apply:** every problem is listed. Fix them all, then
  `siphon apply -f … --dry-run` again. Each error starts with
  `<kind>/<name>:`.
- **"can only be set in siphon.yaml":** an operator-only field:
  - stdio commands;
  - new private hosts;
  - `egress.enabled: false`;
  - `server.*`;
  - AWS roles that aren't allowlisted.

  Ask the operator.
- **"refusing a secret value on the command line":** use `--secret k=-`
  (stdin) or `k=@file`.
- **"private address … private_endpoints":** a LAN or loopback URL must be
  listed by the operator (`server.models.private_endpoints` or
  `server.services.private_endpoints`).
- **"egress proxy … address already in use":** two Siphon instances on one
  host. Give the second its own `server.egress.listen` (for example
  `127.77.0.1:3139`).
- **exit 5 (conflict):** someone changed the item since you read it. Run
  `siphon get` again and re-apply.
- **"not logged in":** `siphon login <url>`, or set `SIPHON_URL` and
  `SIPHON_TOKEN_FILE`.

## Logs and state

- **The daemon:** `journalctl -u siphon -f` (NixOS), or the container's
  logs.
- **A run's units:** `journalctl -u 'siphon-action@*'` and
  `journalctl -u 'siphon-mcp@*'` (MCP bridges).
- **Audit:** `siphon get audit --rule <rule> --since 1h` shows every fire,
  skip, approval and config change.
- **History:** `siphon history` shows every portal or CLI change, with
  `siphon restore <rev>` to undo.
