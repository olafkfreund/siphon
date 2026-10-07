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
      - { id: notify, if: 'steps.summary.exit == 0', cmd: [curl, -fsS, -d, "{{.steps.summary.output}}", https://ntfy.sh/your-topic] }
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

**Schedules:** Siphon has no clock source. A systemd timer (or cron)
POSTs the webhook; `nightly-report`'s header has the NixOS snippet.
