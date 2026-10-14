---
status: draft
issue: 89
author: olafkfreund
---

# Intent: Azure DevOps service hooks trigger rules

## Problem

Azure DevOps is "Not yet" in the catalog. Its reason reads "service hooks
use basic auth only, and the remote MCP is Entra OAuth only". The first
half is out of date.

- **Webhooks.** An Azure DevOps **Web Hooks** service-hook subscription
  (Project settings → Service hooks → Web Hooks) has three settings that
  matter here:
  - **Basic authentication username and password.** Siphon would have to
    verify these, which is weak and puts a password on the wire.
  - **HTTP headers:** free-form `Name: value` lines sent with every
    delivery. That is a shared-token header, which Siphon's existing
    `token` mode already verifies.
  - **"Resource details to send"** (All, Minimal or None), which controls
    payload size.

  So **inbound needs no new signature code:**
  - a `token`-mode source with a header such as `X-Siphon-Key`;
  - the token pasted into the subscription's HTTP headers;
  - a catalog entry and setup text.

  What's missing is the catalog entry, the setup text, a replay key
  (deliveries carry `notificationId`, retried with the same value), and
  templates.
- **Agent tools.**
  - Microsoft's Azure DevOps MCP server is the remote one, OAuth with
    Microsoft Entra. Entra doesn't do dynamic client registration, so it
    needs a pre-registered app (client ID, secret, tenant issuer).
    Siphon's `auth.oauth` can take a fixed client ID and secret, but
    whether that works with Entra is untested.
  - There's also the local `@azure-devops/mcp` server, which uses a
    personal access token. It isn't in nixpkgs, so it would be a new
    pinned package.

## Proposed outcome

- **An `azure-devops` catalog entry:**
  - it creates `<name>-hooks`: `token` mode with `token_header:
    X-Siphon-Key`, a generated secret, and `id: body.notificationId` if the
    id rules allow a body path. Otherwise duplicates are keyed by the body
    hash;
  - **setup text:** create a Web Hooks subscription for the event (work
    item created, build completed, PR created, …), set the URL, and add
    `X-Siphon-Key: <secret>` under HTTP headers. Leave basic auth empty.
    Note that "Resource details to send: Minimal" keeps payloads small;
  - **status `available`,** with webhooks only for now.
- **Templates:**
  - a build that completes with result failed sends a notification;
  - a new work item of type Bug sends a notification.
- **Agent tools are deferred,** with the catalog note updated to name the
  two paths (Entra OAuth for the remote server, or a PAT for the local
  one).

## Affected users and systems

- Azure DevOps users.
- `internal/catalog/services.yaml`, the docs and templates. Possibly
  `internal/config` and `internal/source`, if a body-path delivery id is
  needed for `notificationId`.

## Constraints

- **No basic auth.** Siphon doesn't verify or recommend it.
- **The token travels in a header on every delivery.** That's the existing
  `token` mode, with its existing "weaker than an HMAC" warning. Azure
  DevOps offers nothing stronger for web hooks.
- **No new dependency.**

## Open questions

- **Agent tools now or later?**
  - **Remote MCP** through `auth.oauth` with an Entra app registration
    needs an Azure tenant to test.
  - **The local PAT server** needs packaging `@azure-devops/mcp` in Nix.

  **I'd ship webhooks now** (no new code) and open a follow-up for agent
  tools. Do you have an Azure DevOps organisation and tenant to test
  against?
- **The replay key:** today `id:` reads a header (`header.X-…`), and Azure
  DevOps puts `notificationId` in the body. **I'd add a small `body.<path>`
  option to `id`,** because it's useful for other services too.
  Otherwise the body hash dedupes byte-identical retries, which Azure
  DevOps sends, so it also works. Which do you prefer?
