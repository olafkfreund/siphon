---
status: approved
issue: 29
author: olafkfreund
---

# Intent: CLI-first management and a task-oriented user guide

## Problem

The owner expects most users to **add things from the CLI** and to use the
**portal to verify and manage** them. Siphon isn't built for that yet.

- **The CLI can't add or change anything.**
  - It can validate, dry-run a rule, run once, list jobs, approve or deny,
    import a login, and export the config.
  - It can't create, edit, enable/disable, delete or restore any of the
    things a user works with:
    - sources (webhooks, polled APIs, MCP servers);
    - rules;
    - agents;
    - routines;
    - connections (model endpoints, subscriptions, API keys);
    - the GitHub/GitLab/AWS service presets.
  - Today that means hand-editing `siphon.yaml` and restarting, or using the
    portal.
- **The CLI talks to the database file, not to the running Siphon.**
  - Every command needs `-config <file>`.
  - On NixOS it must run as the service user, against a config path in the
    Nix store:
    `sudo -u siphon siphon jobs ls -config /nix/store/…-siphon.yaml`.
  - It can't be used from another machine, or from a container or microVM
    host.
  - Yet the daemon already has a complete, authenticated HTTP API for all
    of this (`/api/config/{kind}/{name}` with history and restore, plus
    jobs, approvals and rule toggles). That API enforces the same safety
    rules as the portal, but nothing uses it from the command line.
- **The guided setups are portal-only.** Connecting GitHub, GitLab, AWS, a
  model endpoint, or a Claude/Codex/agy login has a careful flow in the
  portal (generated webhook secrets shown once, tool allowlists, Test
  buttons) and no CLI equivalent.
- **There's no "how do I…" documentation.**
  - The README (about 770 lines) is ordered by feature, not by what a user
    wants to do.
  - There's no single place that answers:
    - "how do I make an agent react to a GitHub pull request";
    - "how do I run a command when an API value crosses a threshold";
    - "how do I connect Ollama";
    - "how do I see why my rule didn't fire".
  - Words like source, rule, action, agent, routine, connection and service
    are never introduced together.
  - What users call **a task** — something that watches X and does Y — is
    spread across three or four config sections.

## Proposed outcome

1. **A CLI that manages a running Siphon.**
   - It talks to the daemon's API (address and token from flags, env, or a
     small client config), from the same machine or another one.
   - It can list, show, add, edit, enable/disable, delete and restore
     every kind of item:
     - sources, rules, agents and routines;
     - connections (model endpoints, logins, API keys);
     - services.
   - Changes take effect live, and show up in the portal's history like
     portal edits, under the CLI user's name.
2. **One obvious way to add a task** from the CLI. A user can create the
   whole "watch X → when Y → do Z" chain in one step, from a short file or
   from guided prompts. They can then check it from the CLI ("show me",
   "test it against this event", "why didn't it fire") and see it in the
   portal.
3. **Guided connections from the CLI**, matching the portal:
   - GitHub, GitLab and AWS services;
   - model endpoints;
   - Claude/Codex/agy logins.

   Each comes with the same one-time secret display, the same safe
   defaults, and the same Test.
4. **Same safety as the portal.**
   - The CLI can do nothing the portal can't: the file-only fields stay
     file-only, secrets are write-only, the AWS allowlist and all other
     overlay rules apply, and every change is audited.
   - Secrets are never echoed or put in shell history by default (read from
     stdin or a file).
5. **A user guide organised by task.** It covers:
   - a short concepts page that names every moving part once;
   - "getting started in 10 minutes";
   - how-tos for each common task and connection;
   - troubleshooting.

   Every how-to shows the **CLI** way first, then how to **verify it in the
   portal**, then the **YAML** equivalent for those who manage
   `siphon.yaml` in git.

   The guide's examples are **checked in CI**, so they can't rot. The README
   shrinks to an overview that links into the guide.
6. **Built for LLMs, both ways** (added by the owner on 2026-10-07, during
   the spec: "a cli that is really easy to manage with llms and use llms to
   create new actions and rules. Well documented and clear in use"):
   - **An AI assistant** (Claude Code, Codex, …) can drive the CLI reliably:
     - predictable, non-interactive commands;
     - machine-readable output and errors;
     - self-description of every command and item format, with examples;
     - a short reference written for LLMs.
   - **Siphon itself can use an LLM** to turn a plain-language description
     ("when a PR is opened on repo X, have an agent review it") into a valid
     task. The result is checked by Siphon's own validation, shown to the
     user, and applied only after they confirm.
7. **The existing local commands keep working** (`validate`,
   `rules test`, `run-once`, `serve`, `credentials import`), so nothing
   breaks.

## Affected users and systems

- Everyone who sets up or runs Siphon: the owner, and future users on
  NixOS, the microVM, or the container image.
- The `siphon` CLI (`cmd/siphon`), the HTTP API (only where a CLI need
  exposes a real gap), the docs (`README.md`, a new `docs/` user guide),
  and CI (checking the guide's examples).
- The portal: unchanged in function. It's where CLI changes are verified
  and managed.

## Constraints

- **No new privileges.** The CLI is a client of the existing API and its
  rules. If the CLI needs something the API lacks (for example the
  service presets), it is added to the API with the same checks as the
  portal form.
- **The token is handled like a secret:** never printed, never in argv by
  default, and stored 0600 if the CLI remembers it.
- **Backward compatible:** existing commands and flags keep their meaning;
  a NixOS user with today's `sudo -u siphon … -config` habits isn't broken.
- **Plain Go, no new heavy dependencies** (no CLI framework unless it
  clearly pays for itself).
- **Docs stay true:** every CLI example in the guide is run in CI against a
  test daemon, or validated, so drift fails the build.
- The org workflow (intent → spec → plan) and the coder/reviewer split
  apply.

## Open questions

Resolved by the owner on 2026-10-07 ("approved"): all four proposals below are adopted.

1. **What is a "task" in the CLI?** Proposal: a single file (or a guided
   prompt) holding source + rule + action together. The CLI splits it into
   the existing items, so the portal shows the parts, plus a "task" view
   that groups them. Alternative: no new concept, just document the three
   steps clearly.
2. **Command style.** Proposal: a small set of verbs over kinds, like
   `siphon get|show|add|edit|delete|enable|disable|history|restore <kind> [name]`,
   plus `siphon apply -f file.yaml` for declarative use. Also guided
   helpers for connections: `siphon connect github|gitlab|aws|model|login`.
   Alternative: one command per kind (`siphon source add …`).
3. **How the CLI finds Siphon.** Proposal: `SIPHON_URL` and
   `SIPHON_TOKEN_FILE` (or `siphon login <url>`, which stores both 0600 in
   `~/.config/siphon/client.yaml`). On NixOS, the module also puts the
   binary on PATH with a default that reaches the local daemon, so
   `siphon get rules` just works.
4. **Docs format.** Proposal: Markdown in `docs/` (rendered by GitHub),
   with no site generator for now. A static site can come later.
