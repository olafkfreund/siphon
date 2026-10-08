# Run a task on a schedule

**Goal:** run a command, a routine or an agent at set times (every weekday
at 07:30, every Monday at 09:00, every 15 minutes) without a host timer.

## CLI

```sh
siphon new task --name standup --schedule "30 7 * * 1-5" --timezone Europe/London \
  --cmd '["curl","-fsS","-d","Standup in 30 minutes","https://ntfy.sh/your-topic"]' --yes
```

Or start from a template:

```sh
siphon template weekday-standup     # 07:30 on weekdays, a command
siphon template weekly-digest       # Mondays 09:00: fetch → local-model summary → phone
siphon template nightly-report      # every day 06:00, a routine
```

**Test without waiting** for the clock:

```sh
siphon test standup --at "2026-10-12 07:30"     # builds the event for that moment, runs nothing
siphon why standup                               # shows the next and last run
```

## Writing `at:`

| `at:` | Runs |
|---|---|
| `"30 7 * * 1-5"` | 07:30, Monday to Friday |
| `"0 9 * * 1"` | 09:00 every Monday |
| `"0 6 * * *"` | 06:00 every day |
| `"0 */4 * * *"` | every 4 hours, on the hour |
| `"0 0 1 * *"` | midnight on the 1st of each month |
| `"@daily"`, `"@hourly"`, `"@weekly"`, `"@monthly"` | the usual shortcuts |
| `"every 15m"` | every 15 minutes, from when the source started (at least 1m) |

The five fields are *minute hour day-of-month month day-of-week*
(standard cron, no seconds field).

## The source

```yaml
sources:
  weekly:
    type: schedule
    at: "0 9 * * 1"
    timezone: Europe/London       # IANA name; default: the host's zone
    catch_up: latest              # latest (default) | none
    data: { kind: weekly-digest } # optional, merged into the event
```

The event a rule sees:

```json
{"schedule": "0 9 * * 1", "timezone": "Europe/London", "scheduled_at": "2026-10-12T09:00:00+01:00",
 "fired_at": "2026-10-12T09:00:00.04+01:00", "catch_up": false, "kind": "weekly-digest"}
```

A rule on a schedule source defaults to `on: each` with
`id: event.scheduled_at`: each scheduled moment fires **once**, even if
Siphon restarts. Use `data:` to let one schedule feed several rules
(`when: 'event.kind == "weekly-digest"'`).

## Missed runs

If Siphon was down at a scheduled time:

- **`catch_up: latest`** (the default) runs the **most recent** missed time
  once, when Siphon starts, with `event.catch_up: true`. Older missed
  times are skipped, and recorded in the audit (`schedule_skipped`).
- **`catch_up: none`** skips them all, and records that too.

A new schedule never fires for times before it existed.

## Time zones and daylight saving

Times are in the source's `timezone`. Around clock changes:

- **In spring,** a time inside the skipped hour doesn't run that day.
  For example `30 1 * * *` in Europe/London on the last Sunday of March.
- **In autumn,** a time in the repeated hour runs **once**.

If a job must run every day without exception, use `timezone: UTC`, or a
time outside 01:00–03:00.

## Agents on a schedule

Agents cost money or local compute. Siphon warns when an agent would run
more often than every 15 minutes. `limits.agent_runs_per_day`,
`cooldown` and approvals (on by default) still apply.

## Verify

- **Portal:** **Sources** shows each schedule's **next run** and last run,
  in its time zone. **Jobs** shows each run.
- **CLI:** `siphon get sources` (the NEXT column) and `siphon why <rule>`
  (schedule, next run, last run, and whether a run was missed).

## Troubleshooting

- **It didn't run at the expected time:** check `timezone` in `siphon
  why`. The default is the host's zone.
- **It ran once and never again:** the rule has an explicit `on: edge`.
  Remove it (schedule rules default to `on: each`).
- **It ran late after a restart:** that's `catch_up: latest` doing its
  job. Set `catch_up: none` if a late run is worse than no run.
