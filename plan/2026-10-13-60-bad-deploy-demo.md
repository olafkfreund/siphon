---
status: approved
issue: 60
spec: spec/2026-10-13-60-bad-deploy-demo.md
---

# Plan: a repeatable demo, "a bad deploy, start to finish"

## Approved decisions (from the spec; implement these, nothing else)

- **Two commands.**
  - `nix run .#demo` boots a demo microVM: portal on host
    `127.0.0.1:8091` → guest 8080, notification log on `127.0.0.1:8092` →
    guest 18099, and its own directory
    `SIPHON_DEMO_DIR=~/.config/siphon-demo`.
  - `nix run .#demo-play` plays three acts, pausing for Enter between
    them. `--auto` means no pauses, and the CLI approves.
- **The VM** is `nix/microvm.nix` with a new `demo ? false` argument:
  - stub-model and notify-recv units inside the guest;
  - the two forwards;
  - a first-run wrapper like the existing one that generates `token`,
    `hook` (the GitHub secret) and `kuma` (the X-Siphon-Key secret, also
    used by `morning`), with the same umask, noclobber and symlink
    refusals;
  - then it copies `demo/siphon.yaml` with `@MODEL_URL@` filled in.
- **The model is chosen at every start.** If
  `curl -sf --max-time 2 http://127.0.0.1:11434/api/tags | grep -q '"llama3.2'`,
  the URL is `http://10.0.2.2:11434/v1`. Otherwise it's
  `http://127.0.0.1:8000/v1`. Only the credential's url line is rewritten,
  and the wrapper prints which one it chose. The credential is
  `ollama-local: {provider: openai, url: …}`.
- **`nix/stub-model.py`, additive only:**
  - `/v1/models` lists `stub-model` and `llama3.2`;
  - the GitHub tools `get_workflow_run`, `list_workflow_jobs` and
    `get_job_logs` join `echo`. `get_job_logs` returns the file in
    `argv[1]`, or a one-line log when there's no argument;
  - chat with tools and no tool result yet calls `get_job_logs` if it's
    offered, else the first tool;
  - with a tool result, a prompt containing `Workflow run` gets a fixed
    five-line diagnosis, anything else gets `final: …`;
  - with no tools, a prompt containing `digest` gets five bullets,
    anything else gets `plain answer`.
- **`nix/notify-recv.py`** adds `GET /`, which serves `/tmp/notify.log` as
  `text/plain`.
- **`demo/siphon.yaml`** (operator-only parts):
  - `server`: db, actions_dir, `token: file:/etc/siphon/token`, the egress
    socket, `models.private_endpoints: ["127.0.0.1:8000","10.0.2.2:11434"]`,
    `services.private_endpoints: ["127.0.0.1:18099"]`;
  - `sources.github`: mcp, `http://127.0.0.1:8000/mcp`, `allow_private: true`;
  - `sources.github-hooks`: webhook, `signature: github`,
    `secret: file:/etc/siphon/hook`;
  - `sources.uptime-kuma` and `sources.morning`: webhook,
    `signature: token`, `token_header: X-Siphon-Key`,
    `secret: file:/etc/siphon/kuma`;
  - `credentials.ollama-local`;
  - `notify.phone`: webhook, `http://127.0.0.1:18099/n`, events
    `[approval, failed]`.
- **`demo/tasks/morning.yaml`:** the rule `morning-digest`: source
  `morning`, `when: 'true'`, `on: each`, `id: event.day`, `cooldown: 1m`,
  action `{routine: weekly-digest}`.
- **The templates are applied unchanged**, edited only in the pipe
  `siphon template X | sed … | siphon apply -f -`:
  - `github-ci-failure`: `kind: claude`→`kind: model`,
    `credential: claude-max`→`credential: ollama-local`, plus
    `model: llama3.2`;
  - `uptime-kuma-down`: the ntfy URL →`http://127.0.0.1:18099/kuma`;
  - `weekly-digest`: the fetch URL →`http://127.0.0.1:18099/`, the ntfy URL
    →`http://127.0.0.1:18099/digest`. The schedule is unchanged.
  - Each sed result is grepped for, and the file is dry-run before it's
    applied.
- **`demo/play.sh` (packaged as `demo-play`):**
  - The token comes through `SIPHON_TOKEN_FILE`.
  - The HMAC is computed with `python3 -I` reading the key file, and the
    headers go to curl through `-H @<0600 temp file>`, deleted on exit.
    No secret ever appears on argv or in output.
  - It polls every 2 s with a timeout per act. A timeout exits non-zero
    and names the act.
  - Act 1 is the GitHub `workflow_run` webhook. It waits for
    `pending_approval`; the presenter approves, or `--auto` runs
    `siphon approve`. It waits for `done` and prints the diagnosis.
  - Act 2 is the Kuma webhook. It waits for `done`.
  - Act 3 is `{"day":"<date +%F-%T>"}` sent to `/hook/morning`. It waits
    for the routine to be `done` and prints the digest.
  - Exit prints `siphon why ci-failed` and `siphon test ci-failed --last`.
- **Decisions at spec approval:**
  1. Act 2 is the template's plain notification, not a model summary.
  2. The diagnosis shows in the portal. Only the approval request is pushed.
  3. Act 3 is the `morning` webhook, not a cron.
- **Verification:**
  - `checks.demo` in `nix/demo-test.nix` runs `demo-play --auto` on one
    NixOS test node and asserts the diagnosis, a `get_job_logs` call,
    one `approval`, one `/kuma` and one `/digest` in the notify log, and
    that `morning-digest` fired once;
  - `checks.vm` stays green;
  - `go test ./docs/...` stays green;
  - one manual presenter run on a KVM host, with Ollama and without.
- **Out of scope:** live mode, a recording, changes to any template file,
  a "run now" CLI command.

## Steps

1. **Check the two unverified facts.** No file changes, unless a fact is
   false.
   - **a. Payload fields.**
     - `docs/templates/github-ci-failure.yaml:11-25` reads `x-github-event`,
       `event.action`, `event.workflow_run.{id,name,head_branch,conclusion}`
       and `event.repository.full_name`.
     - `docs/templates/uptime-kuma-down.yaml:7-11` reads
       `event.heartbeat.{status,time}`, `event.monitor.{id,name}` and
       `event.msg`.
     - Compare both with GitHub's documented `workflow_run` payload and
       with Uptime Kuma's webhook notification body (`heartbeat`,
       `monitor`, `msg`; the source is
       `server/notification-providers/webhook.js`).
     - **If a field differs from the real payload**, the template is
       wrong for real users. Stop and raise it with the owner as its own
       issue: the plan does not fork the template. The demo payloads
       always follow the real vendor shape.
   - **b. Job output from the CLI.**
     - `internal/store/query.go:126-132`: `output` is only on
       `JobDetail` (`GetJob`), not on the list rows.
     - So `play.sh` uses `siphon jobs ls -o json` to find the job id and
       state, and `siphon jobs show <id> -o json` for `.output`.
       Confirm it against the real CLI: `go run ./cmd/siphon jobs show 1 -o json`
       against a dev daemon, and `jq 'keys'`.
     - **If `jobs ls` has no `rule`/`state` fields,** filter with
       `-state` and match the rule from `jobs show`.
     - **If `show` has no `output`,** stop: the spec's "prints the
       diagnosis" needs an API change, and that is a deviation for the
       owner.
   - **c. A routine's output.** Find where it lands: on the routine job
     itself, or on child jobs (`parent_id`, `depth`). Grep `routine` in
     `internal/job/`. If it's on children, Act 3 prints the child whose
     step id is `digest`.
   - **d. Literal secrets.** A literal (non-`file:`) `notify.phone.url`
     must validate in `siphon.yaml` (`internal/config/config.go:779`
     resolves only `env:` and `file:`). Run
     `siphon validate -config demo/siphon.yaml`, after Step 5. If it's
     refused, the wrapper writes `config/notify-url` and the yaml uses
     `file:/etc/siphon/notify-url`.
   - **Verify:** write the findings as a "Step 1 findings" note at the end
     of this plan, in the same commit as Step 2.
   - **Traps:** none.

2. **Extend the stubs** (`nix/stub-model.py:15-16,29-31,42-56,57-82,97`
   and `nix/notify-recv.py:5-14`), exactly as in the decisions above.
   - **`notify-recv.py` log format.** The VM test parses every log line
     as JSON with an `event` key (`nix/vm-test.nix:906-907`). So a POST to
     any path other than `/n` and `/k` is logged as
     `{"event":"cmd","path":<path>,"body":<text>}`. `/n` and `/k` keep
     today's raw body. The spec said `GET /` and nothing more; this is
     needed so the demo test can tell `/kuma` from `/digest`.
   - **The digest fetch** is a `GET /` that returns the whole log. That's
     fine: it's what Act 3 summarises.
   - **Verify:**
     ```sh
     python3 nix/stub-model.py demo/fixtures/ci-job.log &
     ```
     Keep `pid=$!`, then:
     - curl `/v1/models`;
     - curl `tools/list` (expect 4 tools);
     - curl `tools/call get_job_logs` (expect the fixture);
     - `kill $pid`.

     Do the same for `notify-recv.py`: POST `/kuma`, then `GET /` shows
     the JSON line. Before Step 4, use a one-line fixture.
   - **Traps:**
     - no `pkill -f`: use `$!`;
     - stdlib only;
     - `/tmp/mcp-calls` logging stays as it is (never the Authorization
       header).

3. **Run `checks.vm` now, before anything depends on the stubs.**
   ```sh
   nix build .#checks.x86_64-linux.vm -L
   ```
   It must pass. If it fails, fix Step 2. Do not touch `vm-test.nix`.
   - **Traps:**
     - needs KVM;
     - the stub stays dual-stack on `::` (`stub-model.py:94-97`), because
       of #57.

4. **Payloads and fixture** (new files):
   - `demo/payloads/workflow-run-failed.json`: a trimmed real-shape
     `workflow_run` event (`action: completed`,
     `workflow_run.conclusion: failure`, `id`, `name: CI`,
     `head_branch: main`, `repository.full_name: acme/shop`);
   - `demo/payloads/kuma-down.json`: `heartbeat.status: 0`,
     `heartbeat.time`, `monitor.id`, `monitor.name: shop.example.com`,
     `msg`;
   - `demo/fixtures/ci-job.log`: about 30 lines of a failing `go test`
     job with one clear cause.

   `play.sh` overwrites `workflow_run.id` and `heartbeat.time` per run, so
   `on: each` sees a new id every time and the demo can be replayed in the
   same VM.
   - **Verify:** `python3 -m json.tool` on both. Run
     `siphon rules test ci-failed demo/payloads/workflow-run-failed.json`
     after Step 5, with `-config` on the rendered yaml and the template
     applied locally, and expect a match. Do the same for `monitor-down`.
   - **Traps:** no real repo names, tokens or emails in the payloads.

5. **`demo/siphon.yaml` and `demo/tasks/morning.yaml`** (new files), as in
   the decisions.
   - **Verify:** render into a scratch directory, with
     `sed 's#@MODEL_URL@#http://127.0.0.1:8000/v1#'` and the `file:`
     paths pointed at scratch secret files (`printf` of random hex, mode
     0600). Then:
     ```sh
     go run ./cmd/siphon validate -config <scratch>/siphon.yaml
     ```
     It should pass, with warnings only. Run the Step 1d check here.
   - **Traps:**
     - no secret values in the repo: only `file:` refs;
     - the egress socket path stays `/run/siphon/egress.sock` (≤107 bytes).

6. **`demo/play.sh`** (new, `set -euo pipefail`), as in the decisions.
   - **Structure:**
     - `send <source> <payload-file> <kind>`: one `python3 -I` call reads
       the payload, stamps the per-run id or time, and writes the body
       file. It reads the key file, computes
       `sha256=<hex hmac>` (github) or uses the key (token), and writes the
       header file (`X-Hub-Signature-256: …` and
       `X-GitHub-Event: workflow_run`, or `X-Siphon-Key: …`), both 0600
       in a `mktemp -d` that a `trap` removes. Then
       `curl -sf -H @hdr --data-binary @body "$SIPHON_URL/hook/<source>"`.
     - `wait_job <rule> <state> <timeout> <act>`.
     - `show_output <id>`, using the form Step 1b/c settled on.
   - **Setup:** for each template, `siphon template X | sed …`. Grep the
     expected edit (`kind: model`, `18099/kuma`, `18099/digest`, `18099/`).
     Then `siphon apply -f - --dry-run -o json`: stop if `errors` isn't
     empty. Then `--yes`. Apply `demo/tasks/morning.yaml` the same way.
     `play.sh` finds `demo/` through `DEMO_SHARE`, which the Nix package
     sets.
   - **Verify:** `shellcheck demo/play.sh`. The run itself is Step 8's
     check.
   - **Traps:**
     - the token only through `SIPHON_TOKEN_FILE`;
     - no secret on argv: not curl's `-H value`, not openssl `-hmac`;
     - never `set -x`;
     - background nothing, and use PIDs if that ever changes;
     - approving only in `--auto` or after the presenter's Enter, which
       follows the AGENTS.md rule that only a human decides to approve.

7. **The VM and the packages:** `nix/microvm.nix` and `flake.nix:39-47`.
   - **`microvm.nix`:**
     - add the `demo ? false` argument;
     - inside the module,
       `lib.mkIf demo { systemd.services.stub-model…; systemd.services.notify-recv…; }`;
     - put the forwards in `microvm.forwardPorts`, with the port values
       chosen by `demo` (8091 and 18099→8092 when it's on);
     - the stub unit's `ExecStart` passes `${../demo/fixtures/ci-job.log}`.
   - **The wrapper** gets `dir=${SIPHON_DEMO_DIR:-$HOME/.config/siphon-demo}`
     when `demo` is on, with the same safety checks. It generates
     `token`, `hook` and `kuma` with `openssl rand`, then copies
     `${../demo/siphon.yaml}`. It runs the model probe on every start and
     `sed -i` rewrites only the `url:` line under `ollama-local`.
   - **`flake.nix`:** `demo = (microvmFor { …; demo = true; }).run;` and
     `demo-play = pkgs.writeShellApplication { name = "demo-play";
     runtimeInputs = [ siphon python3 curl gnused gnugrep coreutils ];
     text = …; }` with `DEMO_SHARE=${./demo}`.
   - **Verify:**
     ```sh
     nix build .#demo .#demo-play
     nix eval .#packages.x86_64-linux.microvm.name
     ```
     `#microvm` must be unchanged. Also `nix flake check --no-build`.
   - **Traps:**
     - merge module settings with `lib.mkMerge`/`lib.mkIf` and
       `lib.recursiveUpdate`, never `//`, for nested attributes
       (`forwardPorts` is a list: build it with `lib.optionals`);
     - no `vendorHash` change, because no `go.mod` change;
     - the 9p share stays read-only.

8. **`checks.demo`:** a new `nix/demo-test.nix`, plus
   `checks = forAll (… demo = import ./nix/demo-test.nix { inherit self pkgs; };)`.
   - **The node:**
     - imports the module, with
       `services.siphon.configFile = "/etc/siphon/siphon.yaml"`;
     - `environment.etc."siphon/siphon.yaml".source` is
       `demo/siphon.yaml`, substituted with `@MODEL_URL@` =
       `http://127.0.0.1:8000/v1`;
     - test-only `token`, `hook` and `kuma` files under `/etc/siphon`;
     - the stub-model and notify-recv units copied from
       `vm-test.nix:36-39,69-72`, with the fixture argument;
     - `demo-play` in `systemPackages`.
   - **The script:**
     - copy the three secret files into `/root/demo/config` (mode 0600);
     - run
       `SIPHON_DEMO_DIR=/root/demo SIPHON_URL=http://127.0.0.1:8080 demo-play --auto`
       and expect exit 0;
     - assert: `jobs show` output contains the stub's diagnosis line;
       `/tmp/mcp-calls` contains `get_job_logs`; the notify log has
       exactly one `"event": "approval"`, one `"path": "/kuma"` and one
       `"path": "/digest"`; and `morning-digest` has one `done` job;
     - grep the journal for the token, hook and kuma values and expect
       none.
   - **Verify:**
     ```sh
     nix build .#checks.x86_64-linux.demo -L
     ```
     Then `nix build .#checks.x86_64-linux.vm` once more.
   - **Traps:**
     - unix sockets: keep the default `/run/siphon` paths, never a
       `$TMPDIR` path (≤107 bytes);
     - nix merges as in Step 7;
     - time: use the polling waits in `play.sh`, no fixed sleeps;
     - no `pkill -f`.

9. **Docs:** a new `docs/demo.md`, a line in `docs/README.md` and one in
   the README's "Ways to run Siphon".
   - **`docs/demo.md` covers:**
     - the two commands and the two ports;
     - what each act shows, and where to click (job page, approve
       button, the :8092 tab);
     - the Ollama-or-stub line;
     - how to replay (`demo-play` again: new ids each run);
     - how to reset (delete `~/.config/siphon-demo`).
   - **Verify:**
     ```sh
     go test ./docs/...
     ```
     It checks links and the embedded docs.
   - **Traps:** no token values in the examples. Use
     `SIPHON_TOKEN_FILE`, never a token on a command line.

10. **The manual presenter run, with the result noted in the PR.**
    - On a KVM host:
      1. `nix run .#demo`, then `nix run .#demo-play` in a second terminal;
      2. approve Act 1 in the portal on :8091;
      3. check the :8092 tab shows the approval, `/kuma` and `/digest`.
    - Do it once with the host Ollama serving llama3.2, and once with it
      stopped (the stub).
    - **Traps:** use `SIPHON_DEMO_DIR`, so the user's own
      `~/.config/siphon-vm` is never touched.

## Tests

```sh
go vet ./... && go test -race ./...        # unchanged Go: must stay green
nix build .#checks.x86_64-linux.vm -L      # after Step 2, and again after Step 8
nix build .#checks.x86_64-linux.demo -L    # Step 8: demo-play --auto passes
nix flake check                            # everything, KVM host
```

Plus the manual run (Step 10).

## Rollback

All the changes are additive: new `demo/`, `nix/demo-test.nix`,
`docs/demo.md`, a new flake package and check, a `demo ? false` argument,
and stub additions. Revert the PR. `#microvm`, the templates and the
daemon are untouched. A user's demo state lives only in
`~/.config/siphon-demo`: delete it.

## Step 1 findings

- **a. Payloads: confirmed.** Webhook headers are lower-cased
  (`internal/source/webhook.go:233`), so `x-github-event` matches. Every
  field `github-ci-failure` reads is in GitHub's `workflow_run` payload.
  Uptime Kuma's `server/notification-providers/webhook.js` sends
  `{heartbeat, monitor, msg}`, which is what `uptime-kuma-down` reads.
  No template issue to raise.
- **b. Job output: confirmed.** `GET /api/jobs` rows (`JobRow`) carry
  `id, rule, state, attempt, created_at, exit_code`; `siphon jobs show <id>
  -o json` is the raw `/api/jobs/{id}` map, so it has `output`.
  `play.sh` uses `jobs ls -o json` plus `jobs show`.
- **c. Routine output: on the routine job itself,** no child jobs. A routine's
  `output` is the JSON progress `{"steps":{"<id>":{"exit","output",…}}}`
  (`internal/job/routine.go:36-41,141`). Act 3 prints
  `.output | fromjson | .steps.digest.output` (python3, no jq needed).
- **d. Literal `notify.phone.url`: refused.** `siphon validate` says
  `inline secret: values must be env:NAME or file:/path`. **Deviation:** the
  wrapper writes `config/notify-url` (`http://127.0.0.1:18099/n`, not a
  secret) and `demo/siphon.yaml` uses `url: file:/etc/siphon/notify-url`.
  Steps 5, 7 and 8 (the test node's `/etc/siphon` files) follow this.

## Deviations

- **Step 2, `/tmp/mcp-calls`:** each line now has ` tool=<name>` before
  `auth-sha=`, so step 8 can assert on `get_job_logs`. The existing
  `vm-test.nix` greps (`hi from the model`, `auth-sha=`) still match.
- **Step 2, the digest text:** the stub's digest tells the demo's night
  (the CI failure, the approved diagnosis, the outage), so it reads well on
  stage. It is canned, and doesn't summarise what it was given.
- **Step 5, `demo/siphon.yaml`:** it also sets `server.listen: 0.0.0.0:8080`
  (the host port forward needs it) and `server.db:
  /var/lib/siphon/state.db`, which the field list didn't name. The
  placeholder is quoted (`url: "@MODEL_URL@"`), so the file is valid YAML
  before the wrapper fills it in. The wrapper's sed must keep the quotes.
- **Step 5, `siphon rules test`:** `ci-failed` only matches with the
  `{"headers": {"x-github-event": "workflow_run"}, "event": …}` envelope.
  The bare payload has no header. Step 8 and `docs/demo.md` use the
  envelope.
- **Step 6, `play.sh` approvals:** interactively the presenter approves in
  the portal only, and the script waits up to 900 s for `done`. Running
  `siphon approve` after a portal approval would fail and abort the demo.
  `--auto` approves with the CLI. A `denied` job ends the act like a
  `failed` one does.
- **Step 6, replays:** `github-ci-failure`'s `cooldown: 5m` suppresses an
  Act 1 replay within 5 minutes, though each run stamps a fresh
  `workflow_run.id` (epoch seconds). `docs/demo.md` says so (step 9).
- **Step 3, result:** `checks.vm` passed on the step 2 stubs
  (`nix build .#checks.x86_64-linux.vm`, 428 s).
- **Step 7, `notify-recv.py`:** it binds to `NOTIFY_BIND` (default
  `127.0.0.1`, as before). The demo VM sets `0.0.0.0` and opens 18099,
  because the SLiRP forward for :8092 reaches the guest's NIC, not its
  loopback. `#microvm` is unchanged: same services, and its firewall still
  opens only 8080 (checked with `nix eval`).
- **Step 8, the check doesn't use `--auto`:** `--auto` approves at once,
  before the notifier's 15 s scan can push the approval, so "one approval
  in the notify log" can't hold. `nix/demo-test.nix` plays it as a
  presenter would: a background watcher waits for the approval line, then
  runs `siphon approve`. Enter keypresses come from a finite `printf`.
  `--auto` stays in `play.sh` for quick, unattended runs.
- **Step 8, the closing `siphon why` / `siphon test --last` are dropped
  from `play.sh`:** `test --last` replays the stored body without headers,
  so header-based rules such as `ci-failed` always report "no match"
  (#65). On stage that read as a failure, and the commands also hung on a
  non-terminal stdin. The script now ends with where to look (the jobs
  page and the notification log). Bring the debug lines back after #65 is
  fixed.
- **Review fixes (after step 9):** a fresh Opus review found the following.
  - **High:** `play.sh` honoured the CLI's own `SIPHON_URL` and
    `SIPHON_TOKEN_FILE`. The NixOS module sets `SIPHON_URL` for every user,
    so setup would have run `apply --yes` against the real daemon. It now
    reads only `SIPHON_DEMO_URL` (default `:8091`) and
    `$dir/config/token`, and unsets `SIPHON_TOKEN`. `checks.demo` runs it
    with a decoy `SIPHON_URL` and `SIPHON_TOKEN_FILE` that must be ignored.
  - **Medium:** the wrapper's Ollama probe matches `"llama3.2:latest"`
    exactly. `llama3.2:1b` alone would have been chosen and then failed
    with "model not found".
  - **Low:** `apply_task` also fails when any `CHANGE-ME` or `example.com`
    URL is left, so every sed edit is checked, not one per template.
    Checked against the current templates.
  - **Not changed:** the 2 s poll race on a very fast approval (the push
    takes up to 15 s), and the plain error messages before the VM is up.
- **Step 10, automated half (the session model):** I ran the real
  `nix run .#demo` VM from the pushed branch with a fresh
  `SIPHON_DEMO_DIR`, then `demo-play --auto`. No host Ollama had
  llama3.2, so it used the stub.
  - The first run created the secrets, and the portal answered on :8091
    after about 4 s. All three acts finished (rc=0), and :8092 showed the
    `/kuma` and `/digest` lines.
  - Found: the diagnosis and the digest printed as the agent's JSON
    envelope (`{"result": "…"}`). `show_output` now prints `.result`. The
    same envelope reaches the phone in every summary template's notify
    step. That's a template bug, now in the scope of #64.
  - The "phone got the notification" line wasn't true in `--auto` mode
    (the approval comes before the 15 s push). It now says "within 15 s".
  - **Still for the owner:** watching it as a presenter (approve in the
    portal, read the pages).
