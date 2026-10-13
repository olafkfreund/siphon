---
status: approved
issue: 65
author: olafkfreund
---

# Intent: siphon test --last and why replay the event's headers

## Problem

When a webhook arrives, Siphon stores its body as the source's last event
(`store.SetSourceEvent`, `internal/store/overlay.go:145`), but not its
headers. `siphon test <rule> --last` (`internal/web/diagapi.go:45-52`) and
`siphon why <rule>` (`diagapi.go:188`) then evaluate the rule against the
body with empty headers.

Any rule that reads a header (`headers["x-github-event"] == …`) is then
reported as "doesn't satisfy when", even though it fired. That covers the
GitHub, Gitea, Forgejo and Bitbucket templates, and most rules users write
for those services. These are the two commands AGENTS.md, the portal and
`docs/llm.md` point people to when a rule misbehaves, so they send people
the wrong way. The #60 demo had to drop its closing debug lines because of
this.

## Proposed outcome

- `siphon test <rule> --last` and `siphon why <rule>` give the same answer
  as the live pipeline did for the same webhook, header-based rules
  included.
- `GET /api/sources/{name}/last-event` shows the stored headers next to
  the body, so the portal's rule tester can use them too.
- No secret is ever stored. Signature, auth and token headers are never
  kept.

## Affected users and systems

- `internal/store` (the last event gains headers; maybe a migration for a
  new column).
- `internal/job/hooks.go:104`, which passes headers when storing.
- `internal/web/diagapi.go` (the test, why and last-event handlers).
- The portal's rule tester, if it uses last-event.
- After this, the #60 demo can bring back its closing debug lines.

## Constraints

- **Secrets:** never store `authorization`, `cookie`, any
  `*signature*`/`*-sig*` header, a source's configured `token_header` or
  `signature_header`, or anything the existing `RedactKeys` would redact.
- The 64 KiB cap on the stored event still holds.
- Old rows without headers keep working, with empty headers as today.
- Polling and schedule sources have no headers, so nothing changes for
  them.

## Open questions

- **Which headers to keep:** a denylist (drop secret-looking ones, keep
  the rest), or an allowlist (keep only known event-type headers such as
  `x-github-event`, `x-gitlab-event`, `x-gitea-event`, `x-forgejo-event`,
  `x-event-key`, `content-type`)? I'd go with a denylist, plus the source's
  own `token_header` and `signature_header`. Rules can read any header,
  and an allowlist would quietly miss custom ones.

## Decisions at approval

- A denylist: drop secret-looking headers plus the source's own `token_header` and `signature_header`, and keep the rest.
