---
status: approved
issue: 33
author: olafkfreund
---

# Intent: A service catalogue, and a redesigned Services page

Evidence:
- [research/2026-10-08-33-service-catalog.md](../research/2026-10-08-33-service-catalog.md):
  28 services, with their signatures, MCP auth and tokens.
- [research/2026-10-08-33-services-design-review-codex.md](../research/2026-10-08-33-services-design-review-codex.md):
  Codex's design review.

## Problem

- **Too few services.** The Services page offers only GitHub, GitLab
  and AWS. The owner wants many more: issue trackers, chat, monitoring,
  cloud and home automation.
- **The page doesn't scale and is hard to use.** Codex's review, read
  against the current screenshot:
  - **Forms first, connections last:** every connect form is open at once
    and fills the first screen. What's already connected sits at the
    bottom.
  - **Connections split into parts:** one GitHub setup shows as separate
    MCP and webhook cards, with no status and no last event. Test is
    missing for webhooks, with no explanation.
  - **Unavailable services waste space:** AWS takes a whole card just to
    say it isn't installed.
  - **Small form bugs:**
    - the webhook checkbox sits above its label;
    - a validation error appears at the top of the whole page;
    - the form forgets what you typed.
  - **No way to scan:** at 20–30 services there's no search, no
    categories, and no distinction between available, connected and
    unavailable.
- **Adding a service is code each time.** Every preset is hand-written
  Go plus a hand-written form. Siphon's webhook checks don't cover some
  popular services (Slack, Stripe, PagerDuty's signature list, Grafana's
  timestamped signature). Remote MCP auth is Bearer-only, but Atlassian,
  PagerDuty and Datadog use other header schemes.

## Proposed outcome

1. **A redesigned Services page** (following the Codex review):
   - **Connected** comes first. Each **connection** is grouped as one
     row (not split into its sources), with:
     - its capabilities (Tools · Polling · Webhooks);
     - an honest status (**Not checked** until a real test has run);
     - its last event;
     - Test (where a test exists) and Edit.
   - **Explore services:** a searchable, category-filtered catalogue of
     compact tiles (code hosting, issue tracking, chat, monitoring and
     alerting, cloud, home and IoT, generic). Each tile shows its
     capabilities and its availability. Services that aren't available
     show why, with no Connect button.
   - **A focused connect page per service,** with numbered steps
     (choose what to connect → provide access → enable events):
     - the permission help sits next to its field;
     - errors appear inline, and what you typed is kept;
     - the one-time secret page stays.
   - Light and dark themes, CSP-clean, and usable on a phone.
2. **One catalogue definition per service** (data, not hand-written Go
   and forms). It covers:
   - category and capabilities;
   - fields and the token guidance;
   - the webhook verification mode and the provider's setup steps;
   - the MCP server: remote URL plus auth header scheme, or a pinned
     local package;
   - the test.

   The portal, `siphon connect <service>`, `siphon draft`, the docs
   and the templates all read it, so adding a service is mostly one
   entry plus a test.
3. **A first wave of about 20 working services**, all usable with a
   static token. The wave and the connection type for each are open
   question 1.
4. **The verification Siphon needs for them:**
   - a `slack` mode and a `stripe` mode;
   - `sha256` options for a signature prefix and list (PagerDuty `v1=`)
     and for the timestamp separator (Grafana `ts:body`).

   Remote MCP servers also get an arbitrary auth header (name and value
   from a secret), not only Bearer.
5. **Honest availability.** Only services Siphon can really verify and
   authenticate are "Available". Others are listed as "Not available
   yet", with the reason (for example "needs OAuth" or "needs Ed25519
   signatures"), so the catalogue doubles as a roadmap.
6. **Tested and documented.**
   - The verification modes are tested against the providers' documented
     examples.
   - Each service gets a connect test against a fake server.
   - The portal and CLI flows are covered, and a VM subtest connects one
     catalogue service.
   - The docs get a services index, generated from the catalogue, plus
     templates for the new services.

## Affected users and systems

- Everyone connecting Siphon to their tools: the owner first.
- **Siphon:**
  - `internal/web` (the Services pages and their API);
  - a new catalogue package;
  - `internal/source` (the verification modes);
  - MCP source auth;
  - `cmd/siphon connect`;
  - the draft prompt;
  - the docs and templates;
  - the NixOS module (pinned local MCP packages, where used);
  - the VM test.

## Constraints

- **No OAuth flows in this task.** Services whose MCP is OAuth-only are
  listed as unavailable for tools. Their webhooks may still be available.
  OAuth becomes its own task.
- **No vendor logos or trademarks:** neutral monograms, as now.
- **The same safety as today:**
  - tokens are write-only;
  - the one-time webhook secret is shown once;
  - stdio MCP commands come only from Nix-pinned `mcp_packages`;
  - private addresses only when allowlisted;
  - the overlay rules apply to the portal, CLI and MCP alike.
- **Signature checks** use constant-time comparison and enforce the
  providers' timestamp windows. Each is tested against the provider's
  documented example.
- **Strict CSP:** no inline styles or scripts. Search and filters work
  without JavaScript, and htmx or `app.js` may enhance them.
- **Backward compatible:** the existing GitHub, GitLab and AWS
  connections and their sources keep working, and are shown as grouped
  connections.

## Open questions

Resolved by the owner on 2026-10-08 ("approved"): all four proposals below are adopted.

1. **The first wave.** Proposed, with how each connects:
   - **Code:** GitHub, GitLab (webhooks + polling; tools need OAuth),
     Bitbucket, Gitea/Forgejo (local MCP in nixpkgs).
   - **Issues:** Linear (remote MCP + webhooks), Jira (remote MCP with a
     Basic header + webhooks).
   - **Chat:** Slack (webhooks only).
   - **Monitoring:**
     - Sentry: webhooks first; tools only if we package the local server;
     - PagerDuty: remote MCP with a custom header, plus webhooks;
     - Grafana: local `mcp-grafana`, plus alert webhooks;
     - Alertmanager, Datadog and Uptime Kuma: webhooks.
   - **Cloud:**
     - AWS (existing);
     - Cloudflare: remote MCP + webhooks;
     - Kubernetes: `mcp-k8s-go`, tools only.
   - **Payments:** Stripe (remote MCP with an Agent key, plus webhooks).
   - **Home:**
     - Home Assistant: built-in MCP + webhooks;
     - ntfy: polling.
   - **Generic:** any webhook, any remote MCP server.

   Discord, Teams, Mattermost, Matrix, Notion, Google Pub/Sub, Event Grid
   and Opsgenie are listed as "not available yet".
2. **Local MCP servers not in nixpkgs** (Sentry, the containers
   Kubernetes server, Azure DevOps): package them in the flake, as for
   AWS, or wait? Proposal: wait. Use what's in nixpkgs, and list the
   rest as "tools need packaging".
3. **Where the connect flow lives:** a dedicated page `/services/<id>`
   (proposed by Codex; works without JavaScript; good for errors and
   keyboards) or a side panel. Proposal: the dedicated page.
4. **Grouping connections:** record which sources belong to one connect
   action (a `connection:` label written by the preset), so Connected
   can group them reliably, instead of guessing from URLs. Proposal:
   yes, an additive label.
