# Approvals

Agent runs wait for a human by default, and any rule or routine step can ask
for approval with `approve: true`.

```sh
siphon get approvals           # what's waiting, and when each expires (24 h)
siphon jobs show 42            # the event and what it would run
siphon approve 42
siphon deny 42
```

- **In the portal:** **Approvals** has the same list, with Approve and
  Deny buttons.
- **The record:** every decision is in `siphon get audit`, with who made it
  (`api:cli:<you>` from the CLI).
- **Expiry:** unanswered approvals expire, and the job is marked as such.
- **Get told:** add a channel so you hear about each approval and get a
  reminder before it expires. See [Notifications](notifications.md).
- **Assistants** (Claude Code, Codex, `siphon mcp`) never approve on their
  own. See [Siphon for AI assistants](../llm.md).
