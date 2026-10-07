---
status: approved
issue: 29
intent: intent/2026-10-07-29-cli-and-guides.md
---

# Spec: CLI-first management and a task-oriented user guide

## Design

Three parts, in order:
- **A. Close the API's gaps,** so it can do everything the portal does.
- **B. A CLI client** of that API.
- **C. A guide whose examples CI runs.**

The CLI gets no privilege of its own. Every write goes through the same
`prepare`/`commit` path (`internal/web/configedit.go:117,161`) as the portal,
so `checkOverlay`'s limits (`internal/config/overlay.go:183-277`) and the audit
apply unchanged.

### A. API additions (`internal/web`)

Today's facts, from research with file:line references:
- PUT takes `{"yaml", "rev"}` and **cannot carry a secret value**: an
  inline value is rejected, and only portal forms write `secrets/` files.
- There is no dry-run.
- Errors are one newline-joined string, with 400 for an invalid config.
- `applied:false` hides the reason.
- Every write is recorded with actor `api` (and restore is mislabelled
  `portal`).
- The service presets, model connections and logins are portal-only, but
  their functions are not tied to `http.Request` (`services.go:138,183,240`,
  `models.go:49,85`, `configweb.go:405,474`).

1. **Write-only secrets on writes.**
   - Item writes accept `"secrets": {"<field path>": "<value>"}`, for
     example `api_key`, `secret`, `env.GITHUB_PERSONAL_ACCESS_TOKEN`,
     `auth.bearer`, `access_key_id`.
   - They become the same `pendingSecret`s the portal forms create:
     written 0600 as `secrets/<kind>-<name>-<field>`, with the item's ref
     set to that `file:`.
   - Unknown or non-secret paths are refused. Values are never echoed or
     logged, and GET still returns refs only.
2. **`dry_run`.** `?dry_run=1` on PUT, DELETE, reset and the new apply runs
   `prepare` and returns `{diff, errors, warnings}` without committing.
3. **Structured errors.** An invalid config returns **422** (as the portal
   does) with `{"error": "<joined>", "errors": ["…", …], "warnings": [...]}`.
   `checkOverlay` collects all its problems instead of stopping at the first.
   `applied:false` adds `"apply_error"`. Auth and 404 errors become JSON too.
4. **Atomic multi-item apply.** `POST /api/config/apply`:
   - body: `{"items": [{kind, name, yaml, rev?}], "delete": [{kind, name}], "secrets": {"<kind>/<name>.<field>": "…"}}`;
   - one `commit` gives one revision: all items apply or none do. This
     is `putItems` (`services.go:111`), which the presets already use;
   - this is what `siphon apply -f` and tasks use.
5. **Actor label.** An optional `X-Siphon-Actor: <label>` header
   (`[\w.@-]{1,64}`) is recorded as `api:<label>`. The CLI sends `cli:$USER`.
   It's a label, not an identity: there's still one shared token, and the
   docs say so. Restore records the real actor.
6. **Connection and service endpoints** (thin JSON wrappers over the
   existing functions). The bodies take the same fields as the portal forms;
   secrets are passed in the body and stored write-only.
   - `POST /api/services/github|gitlab|aws` returns the `serviceDone`
     fields, with JSON tags, including the **one-time** webhook URL,
     secret and header (`Cache-Control: no-store`).
   - `POST /api/services/{name}/test`.
   - `POST /api/connections/models`, and `POST /api/connections/models/{name}/test`
     (test plus the model list).
   - `GET /api/connections`: logins with status and expiry (as
     `credentials ls` shows them), and model endpoints.
   - `POST /api/connections/logins` `{name, provider, kind: login|token|apikey, value}`,
     and `DELETE /api/connections/logins/{name}`.
7. **Diagnostics.**
   - **`POST /api/rules/{name}/test`** takes `{"event": {...}, "headers": {...}}`
     or `{"use_last": true}`, and returns the dry-run fires (argv or
     action) and errors. This needs `runRuleTest` (`editors.go:140`) split
     from its form.
   - **`GET /api/sources/{name}/last-event`** returns the redacted stored
     event (`source_state.json`).
   - **`GET /api/rules/{name}/explain`** gathers in one answer:
     - whether the rule is enabled, or overridden;
     - the source's health and last poll error;
     - when the last event arrived;
     - when the rule last fired, and how much cooldown is left;
     - for `on: edge`, the per-key `last_value` from `rule_state`;
     - whether the last event matches now (a dry run with `use_last`);
     - the rule's last evaluation error;
     - the last webhook rejection;
     - the rule's recent `fire`/`skip_*` audit rows.
   - **Persisted, so `explain` can show them:**
     - a rule's **last evaluation error** (today they're only logged, at
       `queue.go:125`, `hooks.go:57`): a `rule_error(rule, at, error)`
       row, one per rule;
     - a webhook's **last rejection** (status and reason, never the
       body): `last_reject_at` and `last_reject` on `source_state`.
   - **`/api/audit`** gains the filters `rule=`, `event=` and `since=`.
8. **Listing.** `GET /api/config/{kind}` is sorted, and shows tombstones
   (`deleted: true`), as the portal does.

### B. The CLI (`cmd/siphon`)

**Two modes, decided per command, so nothing breaks:**
- **Local mode (today):** any command given `-config`, plus `serve`,
  `run-once`, `validate`, `schema` and `exec-job`. It behaves exactly as now.
- **Client mode:** every other command, and `jobs`/`approve`/`deny` when
  no `-config` is given and a client config exists. It talks to the API.

**Finding Siphon (client mode).** In order of precedence:
1. the flags `-url` and `-token-file`;
2. the env vars `SIPHON_URL` and `SIPHON_TOKEN_FILE` (or `SIPHON_TOKEN`);
3. `~/.config/siphon/client.yaml`: `{url, token_file}`, or `{url, token}`
   written 0600.

`siphon login <url>` reads the token without echo (stdin, or a terminal
prompt), checks it with `GET /api/rules`, and writes `client.yaml` 0600.
`siphon logout` removes it.

**Verbs:**

| Command | What it does |
|---|---|
| `siphon status` | Sources (health), rules (enabled, last fired), pending approvals, recent jobs: the portal dashboard as text |
| `siphon get <kind> [name]` | List (a table), or one item as YAML (`-o yaml\|json`); kinds `sources rules agents routines credentials`, plus `jobs approvals audit` |
| `siphon apply -f <file> [--dry-run] [--secret k=@file\|-]` | Apply a YAML file shaped like `siphon.yaml` (`sources:`/`rules:`/`agents:`/`routines:`/`credentials:`) atomically. It shows the diff first, and `--dry-run` stops there |
| `siphon edit <kind> <name>` | Opens `$EDITOR` on the YAML, then PUT with `rev` (on 409 it explains and re-opens) |
| `siphon delete\|reset <kind> <name>` | Delete a portal item, or reset an override back to the file version |
| `siphon enable\|disable <rule>` | Toggle a rule |
| `siphon history [kind name]` / `siphon restore <rev>` | Revisions, and restoring one |
| `siphon new task` | The guided task wizard (below); `--print` only writes the YAML |
| `siphon connect github\|gitlab\|aws\|model\|login …` | Guided connections (below) |
| `siphon test <rule> [event.json\|--last]` | A remote rule dry-run |
| `siphon test service\|model <name>` | The Test buttons |
| `siphon why <rule>` | `explain`, rendered as a checklist that ends with the likely reason |
| `siphon jobs [ls\|show <id>]`, `siphon approve\|deny <id>` | Jobs and approvals (in client mode) |

**A task** is not a new server concept. It's one apply file holding a
source, an optional agent or routine, and a rule. The portal's Rules page
already shows each rule as source → condition → action (the flow card),
which is the task view, so the portal needs no change.

`siphon new task` asks, in order:
1. **What starts it?** A webhook (GitHub, GitLab, generic), a polled URL,
   an MCP resource, or a schedule-like poll.
2. **When?** Suggests a `when:` expression from the source type, with
   `on: each` or `edge`.
3. **Then what?** A command (argv), an agent (picking an existing one or
   creating one with a prompt, tools and connection), or a routine.

It then prints the YAML, runs a dry-run apply, asks to confirm, applies,
and prints the webhook URL or secret once if it created one. The YAML it
writes is the documented format, so a user can save it and re-apply it from
git.

**`siphon connect`:**
- **Commands:**
  - `connect github`, `connect gitlab`, `connect aws`: the presets;
  - `connect model <ollama|openai|…>`: a model endpoint;
  - `connect login <claude|codex|agy>`: a login file, a setup-token, or an
    API key.
- **Secrets are only read from stdin, a no-echo prompt, or `@file`,
  never from a plain flag.** A `--token sk-…` style flag is refused with
  a hint. One-time secrets are printed once, and are clearly marked.
- **Test:** each connect ends with that connection's Test.

**Output:** a human table or text by default; `-o json` for scripts.
`--quiet` prints only IDs and names. Exit codes: 0 ok, 1 error, 2 usage,
3 validation failed (with every error listed).

**Implementation:**
- stdlib `flag`, plus a small `internal/client` package (`net/http` JSON
  calls and the client config);
- `text/tabwriter` for tables;
- `golang.org/x/term` for no-echo prompts. It's the only new dependency:
  small and official, and stdin is the fallback when there's no terminal.

### B2. Built for LLMs (intent outcome 6)

**For an AI assistant driving the CLI:**
- **Never needs a terminal.** Every wizard has a flag-only form (`siphon new
  task --source … --when … --action …`), `--yes` skips confirmations, and
  stdin is the only interactive input (secrets).
- **Machine-readable everything.**
  - `-o json` works on every command, with documented, stable shapes.
  - Errors on stderr are `{"error", "errors": [...], "hint"}` with exit
    codes 0 ok, 1 error, 2 usage, 3 validation, 4 not found, 5 conflict
    (stale `rev`).
  - A `hint` names the next command to run (for example "run
    `siphon connect github` first").
- **Safe by default.** Every write supports `--dry-run`, which returns the
  diff and the validation result, so an assistant can propose, check, then
  apply.
- **Self-description in one call each:**
  - `siphon help --json`: every command, its flags, args, exit codes and
    one example.
  - `siphon explain <kind>`: the fields of a source, rule, agent, routine
    or credential, with types, required fields, defaults and allowed
    values. It's generated from the JSON schema (`internal/config`'s
    schema generator, already used for `schema/`), so it can't drift.
  - `siphon example <name>`: a minimal valid YAML for each task type and
    item (`webhook-command`, `poll-threshold`, `github-pr-agent`,
    `mcp-watch`, `routine`, `agent`, `model-connection`, …). These are
    the same files the docs use, and CI validates them.
  - `siphon inventory -o json`: the names an assistant may refer to:
    sources, agents, routines, connections (with provider, never secrets),
    `mcp_packages`, `server.aws` allowlists, private endpoints, and rules.
- **A reference written for LLMs:**
  - `docs/llm.md`: the workflow (inventory → example → write YAML →
    `apply --dry-run` → fix → apply → test → why), the rules that commonly
    trip models up (argv, not shell; templates; `on: each` vs `edge`;
    cooldown being mandatory for agents; secrets as refs), and exit codes.
  - A root `llms.txt` that points to it and to `docs/`.
  - `siphon guide` prints `docs/llm.md`, embedded in the binary, so an
    assistant without the repo can read it.
- **`siphon mcp`:** Siphon as a stdio MCP server, so Claude Code or Codex
  can use it natively. It uses the same client config.
  - **Read-only tools:** `inventory`, `get`, `explain`, `example`, `test`,
    `why`, `jobs`, `status`.
  - **Write tools:** `apply` (and `delete`) do a dry-run unless the server
    was started with `--allow-write`.
  - **No approval tools:** approve and deny stay a human's job.
  - Built on the go-sdk, which is already a dependency.

**Siphon using an LLM to write tasks: `siphon draft "<plain-language task>"`**
- **Where it runs:** on the daemon, via `POST /api/draft`, using a
  **model connection** (`ollama`/`openai`, so a local Ollama works at no
  cost; `--model <connection>`, with a default in the client config).
  The model call goes through the existing model client and egress rules.
- **What the model is given:**
  - the item JSON schema;
  - the matching examples;
  - the inventory (names only, **never secrets**);
  - `docs/llm.md`'s rules;
  - the request.
- **The repair loop:** it asks for the apply YAML only, then runs
  `prepare` (dry-run). On errors it feeds them back, up to 3 rounds.
- **What comes back:** the YAML, the diff, any remaining errors, and what
  the user must still do (for example "connect github first",
  "approve: true is set").
- **It never applies by itself.** The CLI prints the result. With
  `--apply` it shows the diff and asks for confirmation (`--yes` for
  scripts), then applies through the normal path.
- **The model's output is untrusted.** Applying it is the same as a user
  applying the YAML: every overlay rule applies. The model can't set
  secrets (it may only reference `connect`ed ones) or file-only fields,
  and agent rules keep `approve: true` unless the user sets otherwise.
- **The draft is recorded:** the request text and model are kept in the
  revision's audit detail as `draft:<connection>`.

### C. NixOS

- `services.siphon.cli.enable` (default `true`) puts the binary on PATH,
  and sets `SIPHON_URL` to the daemon's listen address for all users
  (`environment.variables`).
- The token stays the admin's to give: `siphon login` stores it per user.
- The README's `sudo -u siphon … -config /nix/store/…` advice becomes the
  local-mode fallback.

### D. The guide (`docs/`)

```
docs/
  README.md                  index: start here, concepts, how-tos, reference
  concepts.md                source → rule → action; agents, routines, connections, services, approvals, egress, sandbox, portal vs CLI vs YAML
  getting-started.md         10 minutes: run Siphon (NixOS / microVM / image), siphon login, first task, see it in the portal
  tasks/
    webhook-command.md       run a command when a webhook arrives
    poll-threshold.md        react when a polled API value crosses a threshold (on: edge, repeat)
    github-pr-agent.md       an agent that reviews pull requests (connect github → task → approval)
    mcp-watch.md             watch an MCP resource or tool
    routine.md               multi-step routines
    approvals.md             approve or deny from CLI and portal
  connections/
    logins.md                Claude / Codex / agy subscriptions and API keys
    models.md                Ollama and OpenAI-compatible endpoints
    github.md  gitlab.md  aws.md
  troubleshooting.md         siphon why, last event, rejections, logs
  llm.md                     Siphon for AI assistants: workflow, rules, JSON shapes (also `siphon guide`)
  drafting.md                write tasks in plain language with siphon draft
  cli.md                     every command and flag (checked against the usage text)
  configuration.md           siphon.yaml reference (points at the schema)
  developing/adding-a-source.md   (the existing developer note, moved)
```

- **Every how-to follows one pattern:** Goal → **CLI** (copy-paste) →
  **Verify in the portal** (what you'll see, and where) → **YAML**
  (the same thing in `siphon.yaml`) → Troubleshooting.
- **The README shrinks** to what Siphon is, the ways to run it, a quick
  start, and links into `docs/`. The security notes stay.

**Checked in CI:**
- **YAML blocks:** fenced blocks tagged ` ```yaml siphon ` are extracted
  by a Go test (`docs_test.go`). Each is applied in dry-run mode (or
  validated) against a base config, so a wrong field fails the build.
- **CLI walkthrough:** the getting-started CLI steps and one task per
  how-to (fenced ` ```sh check `) run as a NixOS VM subtest against the
  real daemon:
  - `siphon login`, `connect model` (the stub), `apply -f`, `new task
    --print` | `apply`, `test`, a webhook fire, `why`, `jobs`, `approve`;
  - the output is checked for the documented key lines.
- **`cli.md`:** a test checks that every command in the usage text has a
  `cli.md` entry, and the other way round.

## Alternatives rejected

- **A CLI framework (cobra, urfave):** a big dependency for about 15
  verbs; stdlib `flag` plus a dispatch table, as today, is enough.
- **One command group per kind** (`siphon source add …`): many near-identical
  commands. Verbs over kinds, plus `apply`, cover them with less code and
  less to learn.
- **The CLI writing `siphon.yaml` or the DB directly:** it bypasses the
  overlay rules, audit and live apply; doesn't work remotely; and is the
  very split the intent wants to end.
- **A "task" as a new server concept** (stored, versioned, with its own
  API): its parts already exist and the portal already shows the flow.
  A client-side grouping is free.
- **OpenAPI and a generated client:** a lot of tooling for one client.
  Hand-written calls are small, and the API is stable.
- **A docs site generator (mkdocs, hugo):** deferred (owner question 4).
  GitHub renders Markdown, and links are checked by the docs test.
- **Drafting in the CLI process** (calling the model from the user's
  machine): the model connections and their keys live in the daemon, and
  the daemon has the egress rules. A server endpoint keeps keys server-side
  and works from any client.
- **Drafting with subscription CLIs (claude/codex):** they would need a
  sandboxed agent run, which is slow and heavy for a 3-round repair loop.
  Model connections first (including a local Ollama); a subscription-backed
  draft can come later.
- **MCP write tools on by default:** an assistant could change live
  automation unnoticed. Writes are opt-in with `--allow-write`, and
  approvals are never exposed.
- **Per-user tokens and RBAC:** worth doing, but separate. The actor label
  improves the audit now, and the docs are honest that the token is
  shared and is admin.

## Risks

- **The shared token is full admin over the API, and so over the CLI:**
  the same as the portal today. Mitigations:
  - it's documented;
  - `client.yaml` is 0600;
  - the token is never in argv;
  - auth attempts are rate-limited (existing).
- **Secrets in shell history:** plain-flag secrets are refused, and the
  docs only show stdin, `@file` and prompts.
- **API changes could break the portal.** The new endpoints wrap the same
  functions; the portal routes are unchanged, and the existing web tests
  run.
- **Confusion between the modes:** the rule is simple (`-config` means
  local), and client mode with no URL says exactly how to log in.
- **Docs drift:** the CI checks; `cli.md` is tied to the usage text.
- **`checkOverlay` collecting all errors** changes its control flow. The
  existing overlay tests must keep passing, with the same messages.
- **Model-written tasks may be wrong, or unsafe** (an over-broad `when`,
  shell-like argv, wide tool lists). Mitigations:
  - validation plus `checkOverlay`;
  - the diff shown before applying;
  - `approve: true` kept for agents;
  - the docs telling users to review drafts like any change.

  Prompt injection through inventory names is limited: names match a
  strict regex, and are data inside the prompt.
- **A local model may produce poor YAML.** The repair loop and the
  examples help, and the result is reported honestly when errors remain.
- **Hosts:** none changed. NixOS users gain the binary on PATH and one env
  var.

## Verification

- **Go unit tests:**
  - every new endpoint: auth, CSRF-free bearer, 422 with the list, dry-run
    commits nothing, secrets written 0600 and never returned, atomic apply
    rolling back on a bad item, the actor label, the restore actor;
  - client config precedence and 0600;
  - each CLI verb against an `httptest` daemon;
  - plain-flag secrets refused;
  - local mode unchanged (the existing tests);
  - `explain` reasons: disabled, cooldown, edge already true, eval error,
    rejected webhook, no events yet.
- **LLM features:**
  - `help --json` and `explain` are tested against the usage text and the
    schema;
  - every `siphon example` passes a dry-run;
  - `siphon mcp`: a go-sdk client lists the tools, a write without
    `--allow-write` is only a dry-run, and there's no approve tool;
  - `draft` against a stub model: a bad first answer, a repair with the
    validation errors, then valid YAML; secrets are never in the prompt
    (asserted); and it never applies without `--apply`.
- **Docs tests:** YAML blocks validated, and the `cli.md`/usage parity.
- **NixOS VM subtest:** the getting-started and how-to CLI walkthrough
  against the running daemon (above), plus `cli.enable` putting `siphon`
  on PATH with `SIPHON_URL`.
- **CI:** all existing jobs green.
- **Owner live:** follow `docs/getting-started.md` on p620 from a clean
  shell, then add one real task from the CLI and see it in the portal;
  `siphon draft` one task with the local Ollama; and drive Siphon from
  Claude Code through `siphon mcp`.
