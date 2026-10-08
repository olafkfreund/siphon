---
status: draft
issue: 36
author: olafkfreund
---

# Intent: notifications

## Problem

Siphon tells nobody when something needs a person or has gone wrong.

- **Approvals wait unseen.** An agent action has `approve: true` by default.
  So does a routine step that asks for approval. Siphon records
  `approval_requested` in the audit log
  (`internal/job/approval.go:31`) and logs `approval required` to the journal
  (`internal/job/routine.go:168`). Nothing else happens. The job sits in
  `pending_approval` until someone opens the portal or runs `siphon get
  jobs`. Otherwise it expires (`approval_expired`,
  `internal/store/approval.go:127`), and the work is silently dropped.
- **Failures are silent.** A job that ends `failed` after its retries
  shows up on the dashboard and nowhere else.
- **Broken connections are silent.** The stored connection test (#33,
  `connection_check`) and failing polls (`poll failed` in the journal)
  aren't surfaced outside the portal. A revoked token can break every task
  on a service, and nobody finds out until they look.

Today the only workaround is a hand-written rule. A user can add a `cmd`
that curls ntfy (the `webhook-to-ntfy` template). But that only works for
events that come *into* Siphon, not for Siphon's own state, so it can't
cover any of the three cases above.

The approval default is what keeps agents safe. Without notifications it
also makes them slow and unreliable: a job that needs a click within its
expiry window, and tells nobody, mostly expires.

## Proposed outcome

- **Notified within a minute.** When a job needs approval, fails, or a
  connection stops working, the people who care get a message within a
  minute on a channel they already use. The message says what happened
  and which task it was, and links to the portal page where they can act.
- **Simple to set up.** Setting it up is one CLI command or one portal
  form, for example `siphon notify add ntfy --topic …` or a Slack incoming
  webhook. A test message can be sent from the CLI and from the portal.
- **Controllable.** The user chooses which kinds of event notify which
  channel, and noise is bounded: no message storm when a connection is down
  and a source polls every minute.
- **Reliable.** A notification is not lost across a restart. One that
  can't be delivered is retried, and the failure is visible.
- **Visible in the usual places.** Notifications show up in `siphon why`,
  in the audit log and in the portal, like everything else Siphon does.

## Affected users and systems

- Everyone who runs agents with approvals: the owner today, and future
  users.
- `internal/job` (approval, routine pause, job failure), `internal/store`
  (the audit log, possibly an outbox), `internal/web` and `cmd/siphon`
  (setup and testing), the catalogue (#33: ntfy, Slack and others are
  natural channels), the docs and templates, the NixOS module, and the
  egress allowlist (the daemon gains an outbound path).

## Constraints

- **No new trust path for approvals.** A notification may *link* to the
  authenticated portal. It must not carry a token that approves on its
  own: the one-shot `/a/<id>/<token>` link was rejected earlier because
  tokens are never logged.
- **Secrets stay write-only.** Webhook URLs and tokens for channels are
  write-only secrets, like other credentials: masked in output, errors and
  the audit log.
- **Bounded egress.** Outbound delivery goes only to the configured
  channel hosts. The private-address guard applies unless the operator
  allows it.
- **No loops.** A notification that fails must not produce an event that
  triggers more notifications without limit.
- **No runaway messages.** Rate limiting and deduplication must keep the
  message count bounded under a flapping source.
- **Container installs work too.** It works with `sandbox: none` (the OCI
  image) as well as on NixOS.
- **Same checks as before.** CLI first, `-o json`, exit codes, strict CSP,
  and the existing review gates.

## Open questions

1. **Mechanism.** Either Siphon emits its own events (approval requested,
   job failed, connection failing) as a built-in source, so that ordinary
   rules and actions send the notifications, or Siphon gets a dedicated
   notifier with its own channels. The first reuses rules, templates and
   the sandbox, and needs a loop guard. The second is simpler to set up and
   harder to misconfigure. Proposal: the spec compares both and picks one.
2. **Channels in v1.** Proposal: ntfy, Slack incoming webhook, a generic
   webhook (JSON POST) and email. Is email (SMTP) wanted in v1, or later?
3. **Events in v1.** Proposal: approval requested, approval about to
   expire, job failed, and a connection or source failing for longer than a
   threshold. Is anything else wanted, for example job done for chosen
   rules?
4. **Who gets what.** With users and roles still out of scope, routing is
   by event kind and optionally by rule or tag. Is per-rule routing needed
   in v1?
5. **Reminders.** Should an approval be re-sent before it expires, once,
   at a set fraction of the expiry window?
