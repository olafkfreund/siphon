# Back up, restore and monitor Siphon

**Goal:**
- a backup you can restore on the same host or a new one;
- numbers Prometheus can alert on;
- a choice of how long Siphon keeps its history.

All three are operator settings. You run them on the host, or set them in
`siphon.yaml`. None of them can be changed from the portal or the API.

## What a backup contains

Everything users built lives next to `server.db` (`/var/lib/siphon` with the
NixOS module and in the OCI image):

| In the archive | What |
|---|---|
| `state.db` | tasks and every config revision, jobs, approvals, the audit log, notifications |
| `secrets/` | the secret values pasted in the portal or passed with `--secret` |
| `credentials/` | subscription logins (`siphon connect login`, `credentials import`) |

**Not included:** `siphon.yaml` and `server.token`. Keep those with your own
config management (Nix, /etc, your secrets tool).

**Treat the archive like the secrets themselves.** It holds every token
Siphon was given. Siphon creates it readable only by its owner (mode 0600). Keep it that way, or encrypt it.

## Back up

```sh
siphon backup create -db /var/lib/siphon/state.db /backups/siphon.tar.gz
# backed up to /backups/siphon.tar.gz (1843200 bytes, 4 secrets, 1 logins)
```

- **Siphon keeps running.** The database copy is consistent (SQLite's
  `VACUUM INTO`), even while jobs are being written.
- **Who runs it:** the user that owns the state. That's `siphon` on NixOS:
  `sudo -u siphon siphon backup create …`.
- **`-db` or `-config`:** `-db` points straight at the database. Or give
  `-config siphon.yaml` and the `server.db` in it is used.
- **Existing file:** an existing file is refused unless `--force` is given.

**Encrypted, or straight into a backup tool:** use `-` to write to stdout.
Stdout is refused when it's a terminal.

```sh
sudo -u siphon siphon backup create -db /var/lib/siphon/state.db - | age -r age1… > siphon.tar.gz.age
sudo -u siphon siphon backup create -db /var/lib/siphon/state.db - | restic backup --stdin --stdin-filename siphon.tar.gz
```

**OCI image:**

```sh
podman exec siphon siphon backup create -db /var/lib/siphon/state.db - > siphon.tar.gz
```

### Every day, on NixOS

```nix
services.siphon.backup = {
  enable = true;
  dir = "/var/backup/siphon";   # default
  calendar = "daily";           # systemd OnCalendar
  keep = 7;                     # newest archives kept
};
```

This adds `siphon-backup.timer`, which writes
`siphon-<UTC time>.tar.gz` into `dir` (mode 0700, owned by `siphon`) and
deletes all but the newest `keep`. Run one now with
`systemctl start siphon-backup`. Point your offsite backup at `dir`.

## Restore

Restore works only with Siphon **stopped**, because it replaces the database
underneath it.

```sh
systemctl stop siphon
sudo -u siphon siphon backup restore -db /var/lib/siphon/state.db /backups/siphon.tar.gz
# restore replaces existing state: db 1843200 bytes, 212 jobs, 4 secret files, 1 credential files
# continue? [y/N] y
# restored; the previous state is in /var/lib/siphon/pre-restore-20261008T201500Z; start siphon, then run siphon validate
systemctl start siphon
siphon validate -config /etc/siphon/siphon.yaml
```

- **Checked first:** before anything is changed, the whole archive is
  checked: its names, its size (1 GiB at most), its version and the
  database's integrity. A bad archive is refused and nothing is touched.
- **Nothing is deleted:** the current state is moved into
  `pre-restore-<time>/`. Delete it once you're happy.
- **Refusals:**
  - **While Siphon is running:** exit 5.
  - **As the wrong user** (root on NixOS, for example): refused with the
    user to use.
  - **A backup from a newer Siphon:** refused. Upgrade first.
- **Older backups** are fine. Siphon updates the database when it starts.
- **From stdin** (`… | siphon backup restore … -`): pass `--yes`, because
  stdin can't also answer the question. Without a terminal, `--yes` is
  always needed when state exists.

### Move to a new host

1. On the new host, install Siphon with the same `siphon.yaml` (and the same
   files that its `file:` and `env:` references point to). Don't start it, or
   stop it again.
2. Copy the archive over and restore it as above. The state directory is
   created if it's missing.
3. Start Siphon and run `siphon validate`. Webhook URLs change if the host
   name does: update `server.public_url` and each service's webhook setting.

## Metrics

`GET /metrics` serves the Prometheus text format. It needs the same login
token as the API, so nothing is open without it. A wrong token gets 401, and
repeated failures get 429.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: siphon
    scrape_interval: 30s
    static_configs: [{ targets: ["siphon.lan:8080"] }]
    authorization:
      credentials_file: /run/secrets/siphon-token
```

| Metric | Labels | Meaning |
|---|---|---|
| `siphon_build_info` | `version` | always 1 |
| `siphon_jobs` | `state` | jobs Siphon holds, by state (finished ones for the retention period) |
| `siphon_jobs_finished_last_hour` | `state` | done, failed, cancelled in the last hour |
| `siphon_queue_oldest_seconds` | | age of the oldest queued job (0: none) |
| `siphon_approvals_pending` | | jobs waiting for approval |
| `siphon_approval_oldest_seconds` | | how long the oldest one has waited (0: none) |
| `siphon_agent_runs_today` | | agent runs in the last 24 h |
| `siphon_agent_runs_daily_limit` | | `limits.agent_runs_per_day` |
| `siphon_source_failing` | `source` | 1 while a polled source keeps failing |
| `siphon_source_last_poll_timestamp_seconds` | `source` | when it was last polled |
| `siphon_rule_error` | `rule` | 1 when a rule has an error (see `siphon why`) |
| `siphon_notifications` | `channel`, `state` | notification deliveries by state (`pending` = retrying) |

- **Labels** are only names from your config. Metrics never include job
  contents, errors, URLs or secrets.
- **Computed per scrape:** every value is read from the database when
  Prometheus asks, so restarts don't reset anything.

### Example alerts

```yaml
groups:
  - name: siphon
    rules:
      - alert: SiphonJobFailed
        expr: siphon_jobs_finished_last_hour{state="failed"} > 0
      - alert: SiphonApprovalWaiting
        expr: siphon_approval_oldest_seconds > 72000      # 20 h of the 24 h
      - alert: SiphonSourceFailing
        expr: siphon_source_failing == 1
        for: 15m
      - alert: SiphonNotificationsStuck
        expr: siphon_notifications{state="pending"} > 0
        for: 30m
      - alert: SiphonAgentCapNear
        expr: siphon_agent_runs_today >= 0.9 * siphon_agent_runs_daily_limit
```

[Notifications](notifications.md) already message a person about single
events. Metrics are for dashboards and for alerting through what you
already run.

## How long history is kept

```yaml
# siphon.yaml
server:
  retention:
    jobs: 2160h         # finished jobs with their approvals and notifications (default 720h = 30 days)
    audit: 8760h        # the audit log (default 720h)
    seen_events: 168h   # ids used to drop duplicate webhook deliveries (default 168h = 7 days)
```

- **Units:** values are durations in hours (`h`). Unset or `0` keeps the
  default.
- **Minimum:** each value must be at least `24h`.
- **When it runs:** cleanup runs once a day, so a change applies at the next
  run.
- **Keep the audit log at least as long as you need to answer "who approved
  that?"**
