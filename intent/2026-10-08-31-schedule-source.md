---
status: draft
issue: 31
author: olafkfreund
---

# Intent: Schedule source: run tasks at set times

## Problem

Siphon reacts to things that happen elsewhere: webhooks, polled values,
MCP resources. It has **no clock of its own**. Every "do this every
morning" or "every Friday at 17:00" task today needs:

- **An outside timer:** a systemd timer, cron job or CI schedule whose only
  job is to `curl` a Siphon webhook.
- **A webhook source and secret** for that timer, kept on the host and
  rotated by hand.
- **A workaround in the rule:** `id: event.day` so a timer that fires twice
  doesn't run the task twice.

The `nightly-report` template needs a NixOS snippet of about 6 lines
before it does anything. Users and `siphon draft` have to learn that
detour, and the portal can't show when the next run will be, or whether
one was missed.

Some polled tasks are also really schedules in disguise: `poll: 24h` on an
endpoint, just to run something daily. The poll's timing then drifts with
restarts, rather than landing at a time of day.

## Proposed outcome

1. **A `schedule` source type** emits an event at the times it describes.
   - **The forms people expect:** cron syntax (`"0 7 * * 1-5"`), plus
     plain shortcuts such as `@daily`, `@hourly` or `every 15m`.
   - **A time zone,** so "07:00" means 07:00 local time across DST
     changes.
   - **The event** says when it was scheduled for (`event.scheduled_at`,
     `event.schedule`), so a rule can use the time as its `id`, and a
     scheduled moment can never fire twice, even across a restart.
2. **Missed runs are handled explicitly.** If Siphon was down at a
   scheduled time, the source's setting decides what happens (run the
   latest missed one once, or skip), and the portal shows what happened.
3. **It works like any other source.**
   - Rules, `cooldown`, approvals, `siphon test`, `siphon why`, the
     portal's Sources page (with the **next run** time) and the audit all
     apply.
   - `siphon new task` and the wizard offer "on a schedule".
   - `siphon draft` understands "every weekday at 7".
4. **The scheduled templates get simpler.** `nightly-report` (and any
   future daily or weekly template) uses a `schedule` source, with no host
   timer or webhook secret. The docs gain a "Run a task on a schedule"
   how-to.
5. **Tested and documented.**
   - Unit tests cover parsing, next-run times (including DST and month
     ends), missed-run handling, and restart dedupe, all with an injected
     clock.
   - A VM subtest has a schedule fire a job.
   - The guide, the templates, `llm.md` and `siphon explain source` cover
     it.

## Affected users and systems

- Anyone with recurring tasks: reports, cleanups, digests, periodic
  agent reviews.
- **Siphon:**
  - config: a new source type and its validation;
  - the job pipeline: a scheduler alongside the pollers;
  - the portal's Sources page;
  - the CLI's `new task`;
  - the draft prompt;
  - the docs and templates;
  - the VM test.

## Constraints

- **No new privilege.** A schedule only starts a rule's action, with the
  same sandbox, approvals, cooldown and daily agent cap as any other
  event.
- **Agent safety.** A schedule that runs an agent is bounded by the
  existing `limits.agent_runs_per_day` and `cooldown`, so a mis-typed
  `* * * * *` can't run an agent every minute unnoticed. Validation should
  warn about very frequent agent schedules.
- **Deterministic and testable.** The clock is injectable; tests never
  sleep for real minutes.
- **Plain Go,** with a small, well-known cron parser if one is clearly
  better than writing one. Justify any new dependency.
- **Backward compatible:** existing sources, rules and templates keep
  working. The old timer-plus-webhook pattern stays valid.

## Open questions

Resolved by the owner on 2026-10-08 ("approved"): all four proposals below are adopted.

1. **Syntax:** standard 5-field cron, plus `@daily`/`@weekly`/`@hourly`
   and `every <duration>`? Proposal: yes to all three. No seconds field.
2. **Missed runs:** the default `catch_up: latest` runs the most recent
   missed time once after start-up. The alternatives are `none`, or
   `all` (risky for agents). Proposal: the default is `latest`; `all` is
   not offered.
3. **Time zone:** a per-source `timezone:` (IANA name). Should the default
   be the host's zone or UTC? Proposal: the host's zone, shown explicitly
   in the portal and in `siphon why`.
4. **Payload:** a schedule can carry a fixed `data:` map, merged into the
   event, so one schedule can feed different rules
   (`event.kind == "weekly-report"`). Proposal: yes.
