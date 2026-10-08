# Siphon for AI assistants

This page is for an LLM or coding assistant (Claude Code, Codex, …) that
manages Siphon for a user. `siphon guide` prints it. Read it once before
changing anything.

## What Siphon is

Siphon watches things and acts when a condition holds. A **task** is three
linked parts:

```
source  ──event──▶  rule (when: …)  ──fires──▶  action
```

- **Source.** Where events come from:
  - `webhook`: POSTed to `/hook/<name>`, signed;
  - `http`: a polled JSON URL;
  - `mcp`: a polled MCP resource or read-only tool;
  - `schedule`: fires at set times (`at:` cron, `@daily`, `every 15m`,
    with a `timezone`). Rules on it default to `on: each` with
    `id: event.scheduled_at`. Test one with `siphon test <rule> --at "<time>"`.

  An `mcp` source with no `read` is not polled: it only gives agents tools.
- **Rule.** `when:` is an expression over `event` (and `item` with
  `for_each`, `headers` for webhooks). `on: each` fires once per `id`;
  `on: edge` fires when `when` turns from false to true (with `repeat:`
  re-firing while it stays true).
- **Action.** Exactly one of:
  - `cmd: [argv…]`: a sandboxed command;
  - `agent: <name>`: an AI agent;
  - `routine: <name>`: ordered steps;
  - `unit: <name>.service`: an allowlisted systemd unit.

**Agents** run on a **connection**:
- a subscription login (`kind: claude|codex|agy`);
- a model endpoint (`kind: model`, with an `ollama`/`openai` credential and
  a `model:`).

**Services** (GitHub, GitLab, AWS) are presets that create sources and
connections for you.

## The workflow

Always follow this order. Every step has `-o json`.

1. `siphon catalog -o json`: the services that can be connected on this
   install (and why some can't). If the task needs one that isn't
   connected yet, ask the user to run `siphon connect <id>`, since it needs
   their token.
2. `siphon inventory -o json`: the names that exist (sources, agents,
   routines, connections, MCP packages, allowlists). Reuse them; never
   invent a connection or a secret.
3. `siphon template`: list the ready-made templates (about 30, by
   category), then `siphon template <name>` to start from the closest one,
   for example:
   - `github-pr-review`, `github-ci-failure`;
   - `disk-full`, `upstream-status`, `alertmanager-summary`;
   - `webhook-to-ntfy`, `local-summariser`, `nightly-report`,
     `aws-cloudwatch-alarm`.

   Each template's header lists what to `connect` first and which
   `--secret` flags to pass.

   `siphon explain <kind>` lists every field of a source, rule, agent,
   routine or credential.
4. **Write one apply file** (YAML shaped like `siphon.yaml`: `sources:`,
   `rules:`, `agents:`, `routines:`, `credentials:`).
5. `siphon apply -f task.yaml --dry-run -o json` returns the diff, `errors`
   and `warnings`. Fix every error and run it again.
6. **Show the user the diff**, then `siphon apply -f task.yaml --yes`.
7. `siphon test <rule> --last` (or `siphon test <rule> event.json`) checks
   the rule against a real or sample event.
8. If it doesn't fire later: `siphon why <rule> -o json`.

Instead of steps 3–5, `siphon draft "<what the user wants>"` asks a model
connection to write the apply file and validates it. Review its output the
same way.

## Using Siphon as MCP tools

If your assistant speaks MCP, give it Siphon directly instead of a shell:

```sh
claude mcp add siphon -- siphon mcp                     # Claude Code: read and dry-run only
claude mcp add siphon -- siphon mcp --allow-write       # also let it apply and delete
```

`siphon mcp` uses the same login as the CLI (`siphon login` first). It
offers these tools:
- `inventory`, `get`, `explain`, `template`, `guide`;
- `test`, `why`, `status`, `jobs`, `job`;
- `apply`, `delete`.

Without `--allow-write`, `apply` and `delete` **only dry-run**, even when
asked not to: the result says `"dry_run": true` with a note.

Even with `--allow-write`:
- **Secrets:** secret values, and plain-text header values (which may be
  tokens), are refused unless the user also passes `--allow-secrets`.
  Secrets belong in a terminal (`--secret …=-`), not in a chat.
- **Approval:** turning an agent's `approve` off is refused unless the user
  passes `--allow-unapproved`.
- **No approve or deny tool.**

Changes made this way show in `siphon history` as `api:cli:<user>:mcp`.

## Notifications

`siphon notify add <name> --type ntfy|slack|webhook --url - --yes` (with the URL on stdin)
adds a channel for approvals, reminders, failed jobs and failing sources.
The URL and token are secrets: pass them only with `-` or `@file`. Check the
channel with `siphon notify test <name>`, and see what was sent with
`siphon notify log -o json`. Suggest a channel when a user's agent tasks need
approval: otherwise nobody is told that a job is waiting.

## Rules that commonly go wrong

- **Commands are argv lists, never a shell string.** Write
  `cmd: [echo, "hi {{.event.x}}"]`, not `cmd: "echo hi"`. There's no `sh -c`,
  no pipes, no `&&`. Templates are filled in per element, and a templated
  value may not start with `-`.
- **Templates are Go templates:** `{{.event.field}}` and `{{.item.field}}`.
  The `when:` field is an expression, not a template:
  `event.status == "failed"`, `headers["x-github-event"] == "push"`.
- **`cooldown:` is mandatory** on rules whose action is an agent, or a
  routine containing an agent step.
- **Agents default to `approve: true`:** a human must run
  `siphon approve <job>`. Don't set `approve: false` unless the user asks
  for it.
- **Agents see only the tools listed:** list the tools in `allowed_tools`
  (`mcp__<source>__<tool>`), and their sources in `mcp:`.
- **Secrets are never written in YAML.** A secret field holds a reference
  (`env:NAME` or `file:/path`) in `siphon.yaml`. Through the CLI, pass the
  value with `--secret <kind>/<name>.<field>=-` (stdin) or `=@file`.
  Never put a secret on the command line or in a file you create.
- **Some fields can only be set in `siphon.yaml` by the operator**, never
  through the CLI. If you need one, tell the user:
  - `server.*`, `limits`, `units`;
  - a stdio `command`;
  - `allow_private` for a new host;
  - `egress.enabled: false`;
  - AWS profiles or roles not in `server.aws`.
- **Names:** `[A-Za-z0-9][A-Za-z0-9_.-]{0,63}`. A rule's `name:` must match
  its item name.
- **`on: each` needs a stable `id:`** (a delivery id, an item id).
  Otherwise identical events are deduplicated for 7 days.

## Exit codes

| Code | Meaning | What to do |
|---|---|---|
| 0 | OK | |
| 1 | Error (network, server) | Read `error`; retry if transient |
| 2 | Usage | Fix the command; see `siphon help --json` |
| 3 | Validation failed | Fix every item in `errors`, then dry-run again |
| 4 | Not found | Check the names with `siphon inventory` |
| 5 | Conflict (the item changed since you read it) | `siphon get` it again, re-apply your change |

With `-o json`, errors on stderr are `{"error": "...", "errors": [...], "hint": "..."}`.
`hint` names the next command to try.

## Safety

- Only the user approves agent runs. Don't approve jobs yourself unless
  the user tells you to, for that job.
- `siphon mcp` (Siphon as an MCP server) only dry-runs writes unless the
  user started it with `--allow-write`, and has no approve tool.
- On the CLI, `-o json` never counts as consent. `apply`, `restore` and
  `draft --apply` need an explicit `--yes`, which you add only after the
  user has seen the diff.
- Every change is recorded in `siphon history` with the actor `api:cli:<user>`.
  `siphon restore <rev>` undoes it.
