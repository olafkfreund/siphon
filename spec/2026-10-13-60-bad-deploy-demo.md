---
status: draft
issue: 60
intent: intent/2026-10-13-60-bad-deploy-demo.md
---

# Spec: a repeatable demo, "a bad deploy, start to finish"

## Design

Two commands on the host, both from the flake:

```sh
nix run .#demo          # terminal 1: boots the demo microVM (portal :8091, notifications :8092)
nix run .#demo-play     # terminal 2: plays the story, one act per Enter
```

The demo reuses the microVM, the stubs from the VM test, and the three
templates, applied with `siphon template <name> | sed … | siphon apply -f -`.
That is the same "print, edit the CHANGE-ME parts, apply" route the template
headers tell users to take. No template is copied into `demo/`.

### Layout

```
demo/
  siphon.yaml              # the operator file the VM boots with (no secrets; @MODEL_URL@ placeholder)
  play.sh                  # the replay script (packaged as demo-play)
  payloads/
    workflow-run-failed.json   # a GitHub workflow_run "completed / failure" event
    kuma-down.json             # an Uptime Kuma heartbeat status 0
  tasks/
    morning.yaml           # the morning-digest rule (Act 3's trigger)
  fixtures/
    ci-job.log             # the recorded failing job log the stub MCP serves
docs/demo.md               # how to present it: what to click and say at each act
```

### The VM: `nix/microvm.nix` gets a `demo ? false` argument

With `demo = true`, the existing VM is changed in three ways:

- **Stubs inside the VM.** It adds the same two systemd units as
  `nix/vm-test.nix:36-39` and `:69-72`: `stub-model` (with the path of
  `demo/fixtures/ci-job.log` as an argument) and `notify-recv`. Both listen
  on loopback inside the guest.
- **Ports.** It forwards host `127.0.0.1:8091` → guest `8080` (the portal)
  and `127.0.0.1:8092` → guest `18099` (the notification log). These
  differ from `#microvm`'s 8090, so both VMs can run at the same time.
- **A separate directory.** The wrapper's default is
  `SIPHON_DEMO_DIR=~/.config/siphon-demo`, never the user's `siphon-vm`. On
  first run it does what the existing wrapper does: generate `token` and
  `hook` (the GitHub webhook secret), plus `kuma` (the Uptime Kuma key),
  with the same umask, noclobber and symlink checks. It then copies
  `demo/siphon.yaml` with `@MODEL_URL@` filled in.

`flake.nix` adds `packages.demo = (microvmFor { …; demo = true; }).run`,
next to `microvm`.

### The model: Ollama when it answers, otherwise the stub (decision 1)

At every start, the wrapper runs
`curl -sf --max-time 2 http://127.0.0.1:11434/api/tags | grep -q '"llama3.2'`.

- **If that succeeds,** `MODEL_URL=http://10.0.2.2:11434/v1`.
- **Otherwise,** `MODEL_URL=http://127.0.0.1:8000/v1` (the stub).

It rewrites the credential line in the config, and only that line, so a
presenter's edits survive. The choice is printed in one line ("model: host
Ollama llama3.2" or "model: built-in stub").

`demo/siphon.yaml` declares the credential as
`ollama-local: { provider: openai, url: @MODEL_URL@ }`. That's the name
`weekly-digest` already uses, so the template needs no credential edit.
It also lists both endpoints in `server.models.private_endpoints`.

### The stub answers believably: extend `nix/stub-model.py`

Every change is additive, and the VM test's existing behaviour stays as it is:

- **Model list:** `/v1/models` lists `stub-model` and `llama3.2`.
- **Tools:** `tools/list` adds `get_workflow_run`, `list_workflow_jobs` and
  `get_job_logs` next to `echo`. `get_job_logs` returns the fixture file
  named in `argv[1]`. Without the argument it returns a one-line log, so
  the VM test needs no change.
- **Chat completions:**
  - When it's offered tools and has no tool result yet, it calls
    `get_job_logs` if that's offered. Otherwise it calls the first tool, as
    it does today.
  - When it has a tool result, it answers with a fixed five-line diagnosis
    if the prompt contains `Workflow run`. Otherwise it keeps today's
    `final: …` answer.
  - With no tools, it answers with a five-bullet digest if the prompt says
    `digest`. Otherwise it keeps today's `plain answer`.
- **Calls are still logged:** every `tools/call` is still appended to
  `/tmp/mcp-calls`.

### The operator file: `demo/siphon.yaml`

These are the operator-only parts that a template can't set:

- `server`: the db, actions_dir, `token: file:/etc/siphon/token`, the
  egress socket (as in the microVM's starter config),
  `models.private_endpoints: ["127.0.0.1:8000", "10.0.2.2:11434"]`, and
  `services.private_endpoints: ["127.0.0.1:18099"]`.
- `sources.github`: `type: mcp`, `url: http://127.0.0.1:8000/mcp`,
  `allow_private: true`. It's named `github` because the template's
  `mcp: [github]` and `mcp__github__*` tools expect that.
- `sources.github-hooks`: `type: webhook`, `signature: github`,
  `secret: file:/etc/siphon/hook`.
- `sources.uptime-kuma`: `type: webhook`, `signature: token`,
  `token_header: X-Siphon-Key`, `secret: file:/etc/siphon/kuma`. This is the
  shape `siphon connect uptime-kuma` creates.
- `credentials.ollama-local`, as above.
- `notify.phone`: `type: webhook`, URL `http://127.0.0.1:18099/n`, events
  `approval, failed`. This is the "phone" for this demo.

### `notify-recv.py` gets a `GET /`

It serves `/tmp/notify.log` as `text/plain`, newest line last, so the
presenter keeps `http://127.0.0.1:8092` open in a browser tab next to the
portal. The POST path is unchanged.

### The templates: applied unchanged, edited in the pipe

| Template | Edit in `play.sh` (sed on `siphon template` output) | Why |
|---|---|---|
| `github-ci-failure` | `kind: claude` → `kind: model`, `credential: claude-max` → `credential: ollama-local`, add `model: llama3.2` | Decision 2: the agent runs on the model connection; prompt, tools and rule untouched |
| `uptime-kuma-down` | `https://ntfy.sh/siphon-CHANGE-ME` → `http://127.0.0.1:18099/kuma` | The CHANGE-ME URL is meant to be edited |
| `weekly-digest` | the fetch URL → `http://127.0.0.1:18099/` (the night's notifications); the ntfy URL → `http://127.0.0.1:18099/digest`. The schedule stays as it is. | It digests what really happened in this run |

**Act 3's trigger, added at spec approval:** the routine is started by a
signed webhook, so Act 3 is immediate and deterministic.
- `demo/siphon.yaml` adds `sources.morning`: `type: webhook`,
  `signature: token`, `token_header: X-Siphon-Key`, and the same
  `secret: file:/etc/siphon/kuma`, so there's no new secret.
- `demo/tasks/morning.yaml` holds the rule:

  ```yaml
  rules:
    - name: morning-digest
      source: morning
      when: 'true'
      on: each
      id: event.day
      cooldown: 1m
      action: { routine: weekly-digest }
  ```

  The template's Monday schedule rule stays applied and harmless.

None of the template files change. Each sed is checked: `play.sh` runs
`siphon apply --dry-run -o json` first and stops on any error, so a
template change that breaks a sed fails loudly in the check.
`max_budget_usd` on a model agent is only a warning
(`internal/config/config.go:925`).

Cmd actions without `egress.enabled` run in `siphon-action-open@`, which
denies only the metadata addresses (`nix/module.nix:590`). The template's
`curl` therefore reaches `127.0.0.1:18099` inside the guest.

### `demo/play.sh` (packaged as `demo-play`)

Inputs: `SIPHON_DEMO_DIR` (default as above), `SIPHON_URL` (default
`http://127.0.0.1:8091`), and `--auto` (no pauses, and approves from the
CLI). It sets `SIPHON_TOKEN_FILE=$dir/config/token`, so the token is never
on a command line.

1. **Setup:** applies the three templates as above (dry-run, then `--yes`).
2. **Act 1, "CI fails":** sends `payloads/workflow-run-failed.json` to
   `/hook/github-hooks` with `X-GitHub-Event: workflow_run` and
   `X-Hub-Signature-256`.
   - It computes the HMAC with `python3 -I -c` reading the key file. The
     key is never on argv and never printed.
   - The headers go to curl as `-H @<0600 temp file>`, deleted on exit.
   - It waits for the `ci-failed` job in `pending_approval`. The
     approval notification lands on :8092.
   - Interactive: "Approve it in the portal, then press Enter". `--auto`:
     `siphon approve <id>`, the CLI acting as the presenter.
   - It waits for `done` and prints the job output (the diagnosis).
3. **Act 2, "The site goes down":** sends `payloads/kuma-down.json` with
   `X-Siphon-Key` (the same header-file method). It waits for
   `monitor-down` to be `done`. The notification lands on :8092.
4. **Act 3, "The next morning":** sends `{"day": "<date +%F-%T>"}` (built
   in the script, so each run is a new event for `on: each`) to `/hook/morning` with `X-Siphon-Key`. It waits
   for the `morning-digest` routine job to be `done`, then prints the
   digest.
5. **Exit:** prints `siphon why ci-failed` and `siphon test ci-failed --last`
   as a closing "how to debug" note. Interactive runs leave the rules in
   place.

Job states are read with `siphon jobs ls -o json` and polled every 2 s,
with a timeout per act. A timeout exits non-zero with the act's name.

## Alternatives rejected

- **Change `github-ci-failure` to `kind: model` for everyone.** Real users
  get a better diagnosis from Claude. Decision 2 is about the demo, not the
  template.
- **Copy the three templates into `demo/`.** The intent forbids forking,
  and the copies would drift. Editing in the pipe is checked on every
  `nix flake check`.
- **Add a separate stub for the GitHub tools.** `stub-model.py` already
  speaks MCP. Extending it is smaller.
- **Put `notify-recv` on the host.** Open action units deny `10.0.2.2`
  (`nix/microvm.nix`), on purpose. A forwarded port keeps that rule.
- **A "run the routine now" CLI command.** That's a new feature.
- **The template's cron edited to every minute** (the first draft). It
  meant a wait of up to 60 s on stage. Replaced by the `morning` webhook at
  spec approval.
- **Run the microVM inside the check.** That needs nested KVM. The check
  runs the same config and script on a NixOS test node instead.
- **Have the agent's diagnosis pushed as a notification.** The template
  has no notify step, and adding one changes the template for everyone.
  The diagnosis shows in the portal's job view. The approval request is
  what gets pushed.

## Risks

- **Story deviations from the approved intent.**
  - Act 2 is the template's plain command notification, not a
    local-model summary.
  - Act 1's pushed notification is the approval request. The diagnosis
    itself is in the portal.

  Both come from keeping the templates unchanged. They are open questions
  below.
- **The stub changes could break `checks.vm`.** Every change is additive,
  and the existing `final:`, `plain answer` and `echo` paths stay. The
  plan runs `checks.vm` after the stub step.
- **The sed edits are fragile.** A later template change can silently
  stop a sed from matching. `play.sh` greps for each expected result
  (`kind: model`, the new URLs) before applying, and fails if one is missing.
- **Ollama on CPU** can take a minute per answer. The template's 15-minute
  timeout covers it. The wrapper prints which model it chose, so a slow
  run isn't a surprise.
- **Port clashes on 8091 and 8092.** QEMU fails to start with a clear
  error. Both ports are documented in `docs/demo.md`.
- **Secrets.** The token, hook and kuma files sit 0644 inside a 0700
  directory, as in the existing wrapper. The header temp files are 0600
  and deleted on exit.

## Verification

- **`checks.<system>.demo`**: a NixOS test in `nix/demo-test.nix`, built
  the same way as `nix/vm-test.nix` (one node).
  - The node has `services.siphon` with `demo/siphon.yaml` rendered with
    the stub `MODEL_URL`, the stub and notify-recv units, and generated
    secret files.
  - It runs `demo-play --auto` and expects exit 0.
  - It asserts:
    - `ci-failed`'s job output contains the stub's diagnosis line;
    - `/tmp/mcp-calls` has a `get_job_logs` call;
    - `/tmp/notify.log` holds one `approval` event, one `/kuma` line and
      one `/digest` line;
    - `morning-digest` fired once.
- **`checks.vm`** still passes after the stub change.
- **`go test ./docs/...`** still passes (the template tests). No template
  file changes, so this is a no-op guard.
- **The manual presenter run** on a KVM host, done once before merging
  and noted in the PR:
  - `nix run .#demo` boots;
  - the portal is on :8091 and notifications on :8092;
  - `nix run .#demo-play` walks the three acts with a human approval.
  - Run it once with the host Ollama and once without.

## Decisions at approval

1. Act 2 stays the template's plain notification. It shows the "instant,
   no approval" path next to Act 1's approval.
2. The diagnosis shows in the portal. Only the approval request is pushed.
3. Act 3 is triggered by the `morning` webhook, not by a cron of every
   minute (see "Act 3's trigger").
