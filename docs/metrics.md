# Metrics

Nops serves its metrics at `/metrics` in the Prometheus text format, for
Prometheus to scrape and Alertmanager to alert on.

[Notifications](logs-and-notifications.md#notifications) tell people when a
**deployment** needs them. The metrics cover what a notification cannot:
**Nops alive but stuck**. A git fetch that keeps failing, a store or Nomad
error that aborts every cycle, a file that stopped parsing, an approval
forgotten for days, or a notification adapter that stopped delivering are
ERROR or WARN lines in the log, or a line on the Overview, while `/healthz`
still answers `ok`. Each of them is a metric below, and an alert on it.

## Scraping

`GET /metrics` needs no dashboard session. Under a
[base path](running-nops.md#under-a-sub-path) it answers at the bare path
too, like `/healthz`: Prometheus scrapes the task's own port, not the
reverse proxy that mounts the prefix.

**The token is optional.** With `-metrics-token-file` (or
`NOPS_METRICS_TOKEN`, see [configuration](configuration.md#dashboard)) set, a
scrape must send it as `Authorization: Bearer <token>`, or gets a `401`.
Without one, `/metrics` is open, like `/healthz`. What it shows is the names
of the jobs and namespaces, their policy and whether they drift: no spec, no
diff, no commit, no secret. Set a token when the port is reachable from
anything you would not show that to.

```yaml
scrape_configs:
  - job_name: nops
    authorization:
      credentials_file: /etc/prometheus/nops-metrics-token
    static_configs:
      - targets: ["nops.example.internal:8080"]
```

With Prometheus reading Nomad's service catalog, `nomad_sd_configs` with the
`nops` service of the [example job](running-nops.md#as-a-nomad-job) finds the
task without a fixed address.

**A scrape reads, it keeps nothing.** Every scrape reads the active
deployments from SQLite and the rest from what the last cycles left in
memory (the same state the dashboard shows), so a metric never disagrees
with a page, and a job removed from git leaves the metrics with the next
detection cycle. A scrape that cannot read the store fails with a `500` and
an ERROR in the log rather than answering with the deployments missing:
`up == 0` is the alert for it.

## What is exposed

`namespace` and `job` are the only labels that name a thing, and both grow
only as fast as the repository does. What grows on its own (a deployment
ID, a commit, an error message, a user) would make a new series every time
and stays in the log and on the dashboard.

| Metric | Labels | Value |
|---|---|---|
| `nops_build_info` | `version` | Always 1; `version` is the running build, as in `/healthz`. |
| `nops_loop_last_success_timestamp_seconds` | `loop` | When each loop last did its job, in Unix seconds: `git`, the last poll that reached the remote (whether or not it brought a commit); `detection`, the last cycle that ran to the end (a job skipped for a Nomad error does not abort it, a store error does); `apply`, the last cycle that read the active deployments. Absent until the first one. |
| `nops_deployments` | `state` | Active deployments in each of `detected`, `pending_approval`, `pre_hook`, `applying` and `post_hook`, 0 included. |
| `nops_waiting_since_timestamp_seconds` | `namespace`, `job`, `waiting` | A deployment that waits for a person, and since when: `approval` while it is `pending_approval`, `canary_promotion` while it waits for its canaries to be promoted ([architecture](architecture.md#the-apply-timeout)). Gone once nobody is awaited. |
| `nops_unparsed_files` | | Files the last detection cycle could not parse, or that name a namespace Nops does not manage. |
| `nops_detection_skipped_jobs` | | Managed jobs the last detection cycle could not plan because Nomad failed on them. |
| `nops_orphan_jobs` | | Jobs Nops deployed that are gone from the repository but still run in Nomad ([orphans](architecture.md#orphan-jobs)). |
| `nops_job_info` | `namespace`, `job`, `policy` | Always 1, one per managed job of the last detection cycle; `policy` is the effective one, `none` included. |
| `nops_job_drift` | `namespace`, `job` | 1 if the live job differs from git. |
| `nops_job_paused` | `namespace`, `job` | 1 if a person [paused](deployment-lifecycle.md#holding-a-job) the job. A closed sync window is not a pause. |
| `nops_job_blocked` | `namespace`, `job` | 1 if a failed or rejected deployment holds the job's drift back until a retry or a new commit ([blocked drift](glossary.md#deployment-lifecycle)). |
| `nops_job_invalid_meta` | `namespace`, `job` | 1 if a `nops_*` key has an invalid value, so the job is read as policy `none` ([meta keys](meta-keys.md)). An unknown key is only a warning and does not count. |
| `nops_notifications_failed_total` | `adapter` | Notifications an adapter failed to deliver since Nops started; one series per configured adapter, from 0. |

The three counts of the last detection cycle are absent until the first
cycle has run. The Go runtime's and the process's own metrics (`go_*`:
memory, goroutines, GC; `process_*`: CPU, open file descriptors, start time)
come with them.

## Alerting

A starting point; set the thresholds from your own intervals (a loop's
alert should allow a few of its intervals: detection runs at least every
`-drift-interval`, the git poll every `-git-poll-interval`).

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
