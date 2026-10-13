---
status: draft
issue: 73
author: olafkfreund
---

# Intent: ServiceNow can trigger Siphon rules with signed events

## Problem

Siphon has no ServiceNow connection. A team that runs incidents, changes
and requests in ServiceNow can't have a record change start a rule, a
routine or an agent. They would have to build the sender and work out a
signature scheme themselves.

ServiceNow has no native outbound webhook on the base platform. The
supported way is a Business Rule that calls `RESTMessageV2`, and it signs
nothing unless the script does so. A sender without a signature is
spoofable. Doing this badly also risks loops, where Siphon's own writes
re-trigger the rule, and duplicates, where one record fires on every
update.

This is Phase 1 of the ServiceNow integration (#71).

## Proposed outcome

- **`siphon connect servicenow`** (and the portal's Connections page)
  creates a webhook source for one ServiceNow instance. It prints the
  webhook URL and a signing key, once.
- **A setup guide** gives a copy-paste after-Business Rule that:
  - sends a small, signed JSON envelope for incidents (and any table the
    admin picks);
  - skips changes made by Siphon's integration user;
  - keeps the signing key in a ServiceNow system property, not in the
    script.
- **The delivery is verified** by Siphon's existing Standard Webhooks
  check, with no new signature code. A replayed or tampered delivery is
  refused.
- **Two templates** show the use: an incident notification, and an agent
  that summarises a new incident.
- **Tests prove** that a delivery signed the way ServiceNow's
  `GlideCertificateEncryption.generateMac` signs is accepted.

## Affected users and systems

- ServiceNow administrators, who install the Business Rule.
- Siphon operators and users, through the connect flow, the templates and
  the docs.
- `internal/catalog/services.yaml`, `docs/connections/`, `docs/templates/`,
  `cmd/siphon/describe_test.go` (`baseConfig`), and the generated docs.

## Constraints

- **No new signature kind or verification code.** Reuse
  `signature: standard-webhooks` (`internal/source/webhook.go`
  `verifyStandard`).
- **The signing key is shown once and stored write-only,** like every
  other catalog webhook secret.
- **No ServiceNow API credentials in this phase.** Outbound calls come in
  Phases 2 and 3 (#74, #75). The catalog entry asks only for what inbound
  needs.
- **No basic auth anywhere,** and no IntegrationHub or other licensed
  feature on the ServiceNow side. It must work on a base instance.
- **Event content is untrusted,** even when signed. Templates treat it as
  data.

## Open questions

- **Which event shape does the guide send?**
  - A: a small envelope (`event`, `table`, `sys_id`, `number`,
    `sys_mod_count`, `sys_updated_on`, `changed`, `actor`, plus
    `short_description`, `priority` and `state`), enough for rules to
    match on.
  - B: the whole record. That's bigger, and it can carry personal data.
  - **I'd take A.** An agent that needs more fetches the record in
    Phase 2.
- **Which ServiceNow release do we assume?** The guide targets current
  releases (Washington DC and later). Can you confirm your licensed
  instance's release, so the guide is tested on it?
