# Metrics

Nops serves Prometheus metrics at `/metrics`. Use them to alert when Nops runs
but is stuck: a git fetch that keeps failing, a file that stopped parsing, an
approval forgotten for days. [Notifications](logs-and-notifications.md#notifications)
cover single deployments.

## Scraping

`/metrics` needs no dashboard login. If you set
[`-metrics-token-file`](configuration.md#dashboard), a scrape must send the
token as `Authorization: Bearer <token>`. The metrics hold job names,
policies, and drift. They never hold specs or secrets.

```yaml
scrape_configs:
  - job_name: nops
    authorization:
      credentials_file: /etc/prometheus/nops-metrics-token
    static_configs:
      - targets: ["nops.example.internal:8080"]
```

Under a [sub path](running-nops.md#under-a-sub-path), `/metrics` also answers
at the bare path.

## What is exposed

| Metric | Labels | Value |
|---|---|---|
| `nops_build_info` | `version` | Always 1. |
| `nops_loop_last_success_timestamp_seconds` | `loop` | When the `git`, `detection` or `apply` loop last succeeded. |
| `nops_deployments` | `state` | Active deployments per state. |
| `nops_waiting_since_timestamp_seconds` | `namespace`, `job`, `waiting` | Since when a deployment waits for an `approval` or a `canary_promotion`. |
| `nops_unparsed_files` | | Files the last detection cycle couldn't parse. |
| `nops_detection_skipped_jobs` | | Jobs the last cycle couldn't plan because of a Nomad error. |
| `nops_orphan_jobs` | | [Orphans](architecture.md#orphan-jobs): removed from git, still running. |
| `nops_job_info` | `namespace`, `job`, `policy` | Always 1, one per managed job. |
| `nops_job_drift` | `namespace`, `job` | 1 if the job differs from git. |
| `nops_job_paused` | `namespace`, `job` | 1 if someone paused the job. |
| `nops_job_blocked` | `namespace`, `job` | 1 if a failed or rejected deployment blocks the job. |
| `nops_job_invalid_meta` | `namespace`, `job` | 1 if a `nops_*` key has an invalid value. |
| `nops_notifications_failed_total` | `adapter` | Notifications that failed to deliver. |

Go runtime (`go_*`) and process (`process_*`) metrics come too.

## Alerting

A starting point. Set the thresholds from your own intervals.

```yaml
groups:
  - name: nops
    rules:
      - alert: NopsDown
        expr: up{job="nops"} == 0
        for: 5m
      - alert: NopsLoopStuck
        expr: time() - nops_loop_last_success_timestamp_seconds{loop="git"} > 15 * 60
           or time() - nops_loop_last_success_timestamp_seconds{loop="detection"} > 20 * 60
           or time() - nops_loop_last_success_timestamp_seconds{loop="apply"} > 5 * 60
        annotations:
          summary: "nops: the {{ $labels.loop }} loop has not succeeded for a while; see its ERROR lines"
      - alert: NopsWaitingForAPerson
        expr: time() - nops_waiting_since_timestamp_seconds > 24 * 3600
        annotations:
          summary: "{{ $labels.namespace }}/{{ $labels.job }} has waited for {{ $labels.waiting }} for over a day"
      - alert: NopsCannotReadAJob
        expr: nops_unparsed_files > 0 or nops_job_invalid_meta == 1
        for: 15m
      - alert: NopsNotificationsFailing
        expr: increase(nops_notifications_failed_total[1h]) > 0
```
