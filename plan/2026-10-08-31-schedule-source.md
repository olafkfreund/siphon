---
status: approved
issue: 31
spec: spec/2026-10-08-31-schedule-source.md
---

# Plan: Schedule source

## Approved decisions (self-contained)

- **Source:** `type: schedule` with these fields:
  - `at`: 5-field cron, or `@hourly @daily @weekly @monthly`, or
    `every <d>` (mapped to `@every <d>`, d ≥ 1m), parsed by
    `cron.ParseStandard` from `github.com/robfig/cron/v3` (MIT, no
    dependencies). Only the parser and `Schedule.Next` are used, never
    robfig's runner;
  - `timezone` (IANA; the default is `time.Local`);
  - `catch_up: latest | none` (default `latest`);
  - `data` (a map merged at the event's top level; collisions with the
    reserved fields are a validation error).

  A schedule source has no `url`, `read`, `poll`, `secret` or `command`.
- **Warning:** an agent action (or a routine with an agent step) on a
  schedule more frequent than every 15m warns, citing
  `limits.agent_runs_per_day`.
- **The event:** `{schedule, timezone, scheduled_at (RFC3339 in the source's zone), fired_at, catch_up, …data}`.
- **Firing:** `handleEvent` with `seenScope = "schedule:<name>"` and
  `seenID = scheduled_at.UTC() RFC3339`, so a scheduled moment fires at
  most once, even across restarts. It records
  `source_state.last_poll_at = scheduled_at` and stores the last event.
- **Start-up:**
  - **A new source** (no state row) records `now` and fires nothing for
    the past.
  - **`latest`:** if the latest moment ≤ now is after `last_poll_at`, it
    fires once with `catch_up: true`, and older missed moments are audited
    as `schedule_skipped {source, count, from, to}`.
  - **`none`:** it records and audits any skips.
- **Clock:** injected (`p.Now` plus a sleep/timer seam). Clock jumps
  follow the missed-run rule, and `seen_event` prevents duplicates. A live
  config apply restarts the loops, the same as the pollers.
- **Time zones:** `import _ "time/tzdata"` in the binary.
- **Visible everywhere:**
  - `/api/sources` adds `next_run_at`, `last_run_at` and `timezone` (the
    health is ok/idle, never stale);
  - the portal's Sources page shows the next and last runs with the zone;
  - `siphon get sources` has a NEXT column;
  - `siphon why` shows the next run, the last run, and a missed-run
    line;
  - `siphon test <rule> --at "<time>"`;
  - `siphon new task --schedule "<at>" [--timezone Z]`, plus a wizard
    option;
  - the draft prompt includes the schedule fields and a template.
- **Docs and templates:**
  - `nightly-report` rewritten to `schedule` (`0 6 * * *`), with no timer,
    webhook or secret;
  - a new `weekly-digest` (Monday 09:00: fetch → local-model summary →
    ntfy);
  - a new `docs/tasks/schedule.md`;
  - updates to `concepts.md`, `docs/README.md`, `llm.md`, `llms.txt` and
    the regenerated files.
- **Known DST behaviour,** pinned by tests and documented: a skipped local
  hour doesn't run that day, and a repeated hour fires once.

## Steps

Each step is one commit, citing "Plan step N". Go steps: `go vet ./...` and
`go test -race ./...`. After `go.mod` changes, run
`nix build --rebuild .#siphon.goModules`. Nix steps: `nix flake check`.

| # | Step | Who | Main files |
|---|---|---|---|
| 1 | Config: the schedule type, its fields, validation, the frequent-agent warning, schema and explain hints | coder | `internal/config/config.go`, `explain.go`, `schema/` |
| 2 | The scheduler: loop, fire, catch-up, audit, tzdata; the robfig dependency and `vendorHash` | coder | `internal/job/schedule.go`, `queue.go`, `go.mod`, `flake.nix` |
| 3 | Visible everywhere: API fields, portal Sources, `get sources`, `why`, `test --at`, `new task --schedule` and the wizard, the draft prompt | coder | `internal/web/data.go`, `diagapi.go`, `templates/*`, `cmd/siphon/*`, `internal/draft` |
| 4 | Templates and docs | Opus | `docs/templates/`, `docs/tasks/schedule.md`, `docs/*.md`, `llms.txt`, the generated files |
| 5 | VM subtest: an `every 1m` schedule fires a job, and `why` shows the next run | Opus | `nix/vm-test.nix` |
| 6 | Review (fresh Opus), owner live, PR | Opus | none |

1. **Config.**
   - Add the fields to `Source`, and `"schedule"` to the type switch.
   - Validate with `cron.ParseStandard`, after mapping `every <d>` →
     `@every <d>` and checking `d ≥ 1m`.
   - `time.LoadLocation(timezone)`; the `catch_up` enum; reserved `data`
     keys (`schedule`, `timezone`, `scheduled_at`, `fired_at`,
     `catch_up`).
   - **Polling:** `Polled()` is false for schedule sources (the scheduler
     drives them, not the poll loop). There's no default `poll`, and a
     poll-without-read error must not fire for them.
   - **The warning:** compute the gap between two consecutive `Next`
     calls from a fixed reference time (the minimum over the next 10).
   - **Schema and hints:** regenerate the schema (`UPDATE_SCHEMA=1`), and
     add the explain hints for the new fields; the hint tests must stay
     green.
   - **Tests:** every rule above.
2. **The scheduler.**
   - **Files:** `internal/job/schedule.go`, started from `startPollers`.
   - **Seam:** use the injected clock only, a `p.After(d) <-chan time.Time`
     tests can drive, defaulting to `time.After` on `p.Now`'s timeline.
   - **Behaviour:** fire, catch-up, the audit rows and the state recording,
     as decided above.
   - **Tests** (a fake clock, no real sleeping):
     - Europe/London spring forward (`30 2 * * *` skips) and fall back
       (fires once);
     - month ends (`0 0 31 * *`);
     - a new source gives no past fire;
     - `latest` fires once with `catch_up: true` and audits the skipped
       count; `none` fires nothing;
     - a restart in the same minute gives no duplicate;
     - a live reload keeps the next moment.
   - **Traps:**
     - `go.mod`, then `nix build --rebuild .#siphon.goModules` for the new
       `vendorHash`;
     - the binary must build without host zoneinfo (`time/tzdata`).
3. **Visible everywhere.**
   - `sourceRows` (`internal/web/data.go`) gains `NextRunAt`, `LastRunAt`
     and `Timezone` for schedule sources, from the parser and
     `last_poll_at`.
   - The portal's Sources page shows them, CSP-clean.
   - `cmd/siphon`:
     - the NEXT column in `get sources`;
     - the `why` lines;
     - `test --at` (build the event for that moment, then dry-run);
     - `new task --schedule/--timezone`, and the wizard choice.
   - `internal/draft`: the field tables include `schedule`. Keyword
     matching ("every", "daily", "weekday", "at 7") selects the schedule
     templates.
   - **Tests** for each part.
   - **Trap:** `TestTemplatesAreCSPClean`.
4. **Templates and docs.**
   - Rewrite `docs/templates/nightly-report.yaml` and add
     `weekly-digest.yaml`; both are validated, and the rules tested with
     `test --at`.
   - Write `docs/tasks/schedule.md` in the guide's pattern (Goal → CLI →
     portal → YAML → troubleshooting), covering cron examples, time
     zones, DST, missed runs and agent cost guards.
   - Update `concepts.md` (a fourth source type), `docs/README.md`,
     `llm.md`, `llms.txt` and the templates index.
   - Regenerate `llms-full.txt` and `cli.md`.
5. **The VM subtest.**
   - As alice: `siphon new task --name tick --schedule "every 1m" --cmd '["echo","tick"]' --yes`.
   - Assert a `tick` job is done within 150 s.
   - Assert `siphon why tick -o json` has a `next_run_at` in the future.
   - Assert `siphon get sources -o json` shows `type: schedule`.
6. **Review and PR.**
   - A fresh Opus review against this plan and the diff. Focus: catch-up
     and dedupe correctness, clock seams, agent cost, and DoS through a
     tiny `every`.
   - Owner live: a weekday-morning schedule on the dev instance.
   - Then the PR.

## Tests

`go test -race ./...`, `devenv test`, `nix flake check` (the VM test with
the new subtest), the docs tests, CI green, and the owner's live check.

## Handoff

- **Coder:** steps 1–3, one agent, steps in order via SendMessage.
- **Opus:** steps 4–6. Step 4's docs can be drafted alongside steps 2–3,
  and the templates are validated once step 1 lands.

## Rollback

Revert the merge. The new source type is additive, and existing configs
are unchanged.

## Deviations log
- **Step 1 (coder):**
  - The robfig dependency and the `vendorHash` landed in step 1, not step 2 as the table says.
  - Exported helpers for step 2: `config.ParseSchedule`, `(*Source).Location()`, `ScheduleReserved`, `MinScheduleEvery`.
  - `at`, `timezone`, `catch_up` and `data` are rejected on other source types.
