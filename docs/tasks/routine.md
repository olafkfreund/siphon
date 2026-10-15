# Routines: several steps in order

**Goal:** one trigger, several things: fetch → summarise with an agent →
notify, with retries and conditions.

```sh
siphon template nightly-report > nightly.yaml     # fetch, summarise (local model), push to phone
siphon template alertmanager-summary > alerts.yaml
```

```yaml
routines:
  nightly:
    steps:
      - { id: fetch, cmd: [curl, -fsS, https://metrics.example.com/api/summary], retry: { attempts: 3, base: 5s, factor: 2 } }
      - { id: summary, agent: reporter, if: 'steps.fetch.exit == 0', timeout: 5m }
      - { id: notify, if: 'steps.summary.exit == 0', cmd: [curl, -fsS, --data-raw, "Summary:\n{{.steps.summary.json.result}}", https://ntfy.sh/your-topic] }
```

- **Earlier steps:** later steps can read `steps.<id>.exit`,
  `.output`, `.json` (parsed output) and `.skipped`, in `if:` and in
  templates.
- **`retry`** backs off exponentially. **`continue_on_error: true`** lets
  the routine go on after a failed step.
- **Agent steps:**
  - **Approval:** they wait for approval like any agent. Set
    `approve: false` on the agent only for read-only work, such as a
    summary.
  - **Cooldown:** a rule whose routine contains an agent step needs a
    `cooldown`.

**Schedules:** use a `type: schedule` source (`at: "0 6 * * *"`, `timezone:`), as `nightly-report` does. See [Run a task on a schedule](schedule.md).

**In the portal:** **Routines → New** starts from a starter, or from a recipe
(any template with a routine, `/config/routines/new?recipe=nightly-report`). A
recipe fills in only the routine; the banner lists the template's other items
(sources, agents, rules, …) to add separately. The **Reference** panel beside
the editor lists every step field, and its tokens (`{{.steps.<id>.output}}` …)
insert at the cursor.
