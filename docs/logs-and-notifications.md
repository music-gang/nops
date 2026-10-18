# Logs and notifications

## Logs

Nops logs JSON lines through `log/slog`, with the keys `deployment_id`,
`job`, `namespace`, `state`, `phase`, `hook_job`, `repo` and `commit`.

| Event | Level |
|---|---|
| State transition, canary promotion, pause, resume | INFO |
| Unknown meta key, notification not delivered | WARN |
| Deployment `failed`, invalid meta value, job that doesn't parse | ERROR |
| Nomad or SQLite error | ERROR |

## Notifications

Nops sends a notification when a deployment:

- waits for approval (`pending_approval`);
- waits for someone to promote its canaries;
- fails (`failed`);
- completes, only for jobs with `nops_notify_completed = "true"`
  ([meta keys](meta-keys.md)).

Nops also sends one when someone pauses or resumes every job.

Turn on one or more adapters in [configuration](configuration.md#notifications):

| Adapter | Sends |
|---|---|
| webhook | The JSON below, for n8n, Home Assistant, or your own receiver. |
| Discord | An embed, colored by state. |
| Slack | A message with link buttons. |
| ntfy | A plain-text message with a priority by state. |
| Gotify | A message with a priority by state. |

```json
{
  "event": "deployment",
  "deployment_id": "01J8ZX4N6Q9V3T2K5M7P8R1S0W",
  "job": "web",
  "namespace": "apps",
  "state": "failed",
  "phase": "pre",
  "policy": "approval",
  "approved_by": "alice",
  "error": "pre-hook web-migrate failed: exit code 1",
  "repository": "default",
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "commit_subject": "Bump web to 1.4.0",
  "commit_author": "Alice",
  "commit_url": "https://git.example.com/ops/jobs/commit/0123456789abcdef0123456789abcdef01234567",
  "url": "https://nops.example.com/deployments/01J8ZX4N6Q9V3T2K5M7P8R1S0W",
  "nomad_url": "https://nomad.example.com/ui/jobs/web@apps",
  "time": "2026-09-23T10:00:00Z"
}
```

A notification waiting for a promotion also has `"waiting": "canary_promotion"`.
One waiting for approval also has `changes`, such as `"2 groups, 3 tasks changed"`,
one for a retry has `retry_of`, the ID of the deployment it retries, and one a
person deployed outside its sync window has `deployed_now_by`, who asked.
When someone pauses or resumes every job, `event` is `paused` or `resumed`,
with `actor` and `reason`, and the deployment fields are empty.

Nops tries each notification once, with no retry. A failed delivery logs a
WARN and never stops a deployment. The log never contains the adapter URL,
because Discord and Slack URLs hold a token.
