---
status: approved
issue: 31
intent: intent/2026-10-08-31-schedule-source.md
---

# Spec: Schedule source

## Design

### Config

```yaml
sources:
  mornings:
    type: schedule
    at: "0 7 * * 1-5"            # 5-field cron; or @hourly @daily @weekly @monthly; or "every 15m"
    timezone: Europe/London      # IANA name; default: the host's zone
    catch_up: latest             # latest (default) | none
    data: { kind: standup }      # optional, merged into the event
rules:
  - name: standup-digest
    source: mornings
    when: 'event.kind == "standup"'
    on: each
    id: event.scheduled_at       # optional: schedule events are already de-duplicated per scheduled time
    action: { agent: digest }
```

**Validation** (`internal/config`):
- `at` is required and must parse. `every <d>` maps to cron's `@every <d>`, and `d` must be ≥ 1m.
- `timezone` must load (`time.LoadLocation`).
- `catch_up` is `latest` or `none`.
- `data` keys must not collide with the reserved event fields.
- A `schedule` source has no `url`, `read`, `poll`, `secret` or `command`.
- **Warning:** a rule whose action is an agent (or a routine with an agent
  step), on a schedule that fires more often than every 15 minutes:
  "rules/R: an agent on a schedule more frequent than 15m; check the cost;
  limits.agent_runs_per_day caps it at N".

`siphon explain source` and the JSON schema include the new fields.

### The event

```json
{"schedule": "0 7 * * 1-5", "timezone": "Europe/London", "scheduled_at": "2026-10-08T07:00:00+01:00",
 "fired_at": "2026-10-08T07:00:00.03+01:00", "catch_up": false, "kind": "standup"}
```

- **`scheduled_at`:** the planned moment, in the source's zone, RFC3339.
- **`catch_up`:** true when the event is a missed run fired after
  start-up.
- **`data` keys:** merged at the top level. The reserved fields win, and
  validation forbids collisions anyway.

### The scheduler (`internal/job`)

**Parsing:** `github.com/robfig/cron/v3` (MIT, no dependencies, the parser
Kubernetes CronJobs use).
- Its `cron.ParseStandard` handles 5 fields and the `@` descriptors, and
  `Schedule.Next(t)` computes the next run in the time zone of `t`.
- Its own job runner is **not** used.

**Loop:** `startPollers` also starts a `scheduleLoop` for every `schedule`
source:

```
next := sched.Next(now in tz)
loop: wait until next (timer on the injected clock: p.Now plus a sleep func tests replace)
      fire(next) ; next = sched.Next(next)
```

**`fire(at)`** calls `handleEvent` with `seenScope = "schedule:<name>"` and
`seenID = at.UTC() RFC3339`.
- **Dedupe:** the existing `seen_event` de-duplication (7 days) means a
  scheduled moment fires at most once, even across restarts or overlapping
  instances.
- **State:** it records `source_state.last_poll_at = at` (the last moment
  handled) and stores the event as the source's last event, so `siphon
  test --last` works.

**Missed runs**, at start-up:
- **A new source** (no `source_state` row): record `last_poll_at = now`,
  and fire nothing for the past.
- **`catch_up: latest`:** if the latest scheduled moment ≤ now is after
  `last_poll_at`, fire it **once** with `catch_up: true`. Older missed
  moments are skipped, and the skip is audited as
  `schedule_skipped {source, count, from, to}`.
- **`catch_up: none`:** record and skip. Audit it if anything was missed.

**Clock jumps:** a jump forward (suspend, NTP) behaves like a missed run,
by the same rule. A jump back waits for the next moment, and `seen_event`
prevents duplicates.

**Live config apply** restarts the schedule loops, the same as the
pollers. The next moment is recomputed, so no run is lost or doubled.

**Time zones:** `import _ "time/tzdata"` embeds the IANA database (about
450 KB), so zones resolve in the image and the microVM without host
zoneinfo. The default zone is `time.Local`, and the portal and `why` show
its name.

### Visible everywhere

- **`/api/sources`:** `type: schedule`, with `next_run_at`,
  `last_run_at` and `timezone`. The health is `ok`/`idle`, never `stale`.
- **The portal's Sources page:** "next run 07:00 (Europe/London)" and the
  last run.
- **`siphon get sources`:** a NEXT column.
- **`siphon why`:** "next run …"; "last run …"; and if the last
  scheduled moment was skipped, "missed while Siphon was down (catch_up:
  none)".
- **`siphon test <rule> --last`:** works on the last schedule event.
  `siphon test <rule> --at "2026-10-09 07:00"` builds the event for a
  given moment.
- **`siphon new task --schedule "0 7 * * 1-5" [--timezone Z]`:** creates
  a schedule source. The wizard offers "On a schedule".
- **`siphon draft`:** the prompt's field tables and a schedule template
  teach it "every weekday at 7".

### Templates and docs

- **`nightly-report`** is rewritten: a `schedule` source (`0 6 * * *`),
  with no timer, webhook or secret, so its header shrinks.
- **New `weekly-digest`:** every Monday at 09:00, a routine fetches a
  JSON endpoint you name, a local model summarises it, and the summary is
  pushed to your phone.
- **New how-to `docs/tasks/schedule.md`** covers:
  - cron syntax with examples;
  - time zones and DST;
  - missed runs;
  - testing with `--at`;
  - cost guards for agents.

  It's linked from the index, `llms.txt`, `concepts.md` (a new source
  type) and `llm.md`.

## Alternatives rejected

- **Hand-written cron parser:** about 200 lines, plus the DST and
  month-end edge cases that robfig already gets right and that Kubernetes
  relies on. A zero-dependency MIT library is cheaper and safer.
- **robfig's job runner:** it owns goroutines and its own clock. Siphon
  needs the injected clock, `seen_event` dedupe, live reloads and
  `catch_up`, so only the parser is used.
- **`catch_up: all`:** replaying every missed run after an outage could
  start many agents at once. Excluded (owner question 2).
- **Making `poll:` accept cron:** it mixes two ideas. A schedule has no
  URL and fires at times; a poll fetches data and follows drift.
- **systemd timers generated by the NixOS module:** host-only (not the
  container image), and invisible to the portal and `why`.

## Risks

- **DST:**
  - **A skipped local hour** (spring forward): a 02:30 job doesn't run
    that day. That's robfig's behaviour, and it's documented, with the
    advice to use UTC for jobs that must run daily without exception.
  - **A repeated hour** (fall back): robfig fires once. Tests pin both
    behaviours.
- **Frequent agent schedules can cost money.** Mitigations:
  - the validation warning;
  - `limits.agent_runs_per_day`;
  - `cooldown`;
  - approvals by default.
- **Clock skew between the host and the user's expectation:** the portal
  and `why` always show the zone.
- **Hosts:** none changed. The binary grows by about 450 KB (tzdata).

## Verification

- **Unit tests** (injected clock, no real sleeping):
  - parsing (cron, the descriptors, `every`, and the rejections);
  - next-run computation across DST in Europe/London (both transitions)
    and at month ends;
  - new source → no past fire;
  - `latest` catch-up fires exactly once, with `catch_up: true`, and
    audits the skipped count; `none` fires nothing;
  - a restart at the same moment → no duplicate (`seen_event`);
  - a live reload keeps the next moment;
  - the frequent-agent warning;
  - `data` merging and the reserved-key rejection.
- **API/CLI tests:** `next_run_at` in `/api/sources`; the `get sources`
  NEXT column; `why` lines; `test --at`; `new task --schedule` producing
  valid YAML.
- **VM subtest:** a schedule `every 1m` with a `cmd` action produces a done
  job within about 90 s, and `siphon why` shows a next run.
- **Docs tests:** the new template validates; links resolve;
  `llms-full.txt` and `cli.md` are fresh.
- **Owner live:** a real weekday-morning schedule on the dev instance, seen
  in the portal with its next run.
