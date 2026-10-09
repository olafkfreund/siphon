---
status: draft
issue: 40
author: olafkfreund
---

# Intent: scrape-only token for /metrics

## Problem

`GET /metrics` (#38) only accepts the admin token (`server.token`).
Prometheus, or whatever scrapes Siphon, has to hold that token. It is the
same credential that can do everything else through the API:

- `siphon apply`;
- approving jobs;
- setting secrets;
- restoring config revisions.

Anyone who can read the scrape config, the Prometheus host's credentials
file, or that host's memory can take over Siphon. The #38 security review
raised this as M3. The owner accepted it for #38, and
`docs/tasks/backup-and-monitoring.md` documents the risk ("The token is the
admin token"). This task removes it.

## Proposed outcome

- **A second, read-only token.** The operator can set a separate token that
  opens `/metrics` and nothing else. With it, a scraper gets 401 on every
  `/api` route and on the portal.
- **Easy for Prometheus.** Prometheus is set up with
  `authorization.credentials_file` pointing at that token. The docs' scrape
  example uses it and drops the "admin token" warning.
- **Existing setups keep working.** A scrape config that already uses the
  admin token still works.
- **NixOS:** the module can pass the token as a file, the same way
  `server.token` is passed today (`services.siphon.credentials`, from agenix or sops).

## Affected users and systems

- **Code:**
  - `internal/config` (the `server` block, `Validate`, the schema);
  - `internal/web` (the `/metrics` auth check);
  - `nix/module.nix`, if a module option is added;
  - `nix/vm-test.nix`.
- **Docs:** `docs/tasks/backup-and-monitoring.md` and the generated
  `llms-full.txt`.
- **Operators** with Prometheus. No one else is affected.

## Constraints

- **Operator-only.** The token lives in `siphon.yaml`, like
  `server.token`. It can't be set or read through the portal, the API or
  `siphon apply`, because overlay kinds exclude `server`.
- **Same handling as the admin token:**
  - `file:` or `env:` references;
  - a minimum length of 32 characters;
  - a constant-time compare;
  - the same failed-auth rate limit;
  - never logged.
- **Never interchangeable.** The scrape token must never work on any route
  other than `/metrics`, including the portal login.
- **No new listener or port.**
- **No users or roles system.** That is a separate, later feature (OIDC).
  This is one fixed-purpose token.

## Open questions

1. **Config shape.** There are two options:
   - **(a)** `server.metrics_token: <secret>`;
   - **(b)** `server.metrics: { token: <secret> }`, which leaves room for
     future metrics settings.

   Recommendation: (b). It reads naturally next to `server.retention`, and
   any later metrics option has an obvious home.
2. **Keep the admin token working on `/metrics`?** Recommendation: yes,
   always. Removing it would break every existing scrape config on upgrade.
   The docs recommend the scrape token instead.
3. **NixOS option.** There are two options:
   - a dedicated `services.siphon.metricsTokenFile`;
   - the existing `services.siphon.credentials.metrics-token =
     "/run/agenix/…"` plus `settings.server.metrics.token =
     "file:/run/credentials/siphon.service/metrics-token"`. That is how
     `server.token` is passed today.

   Recommendation: the existing mechanism, with a doc example. It adds no
   option.
