# Logs and notifications

## Logs

Nops logs JSON lines through `log/slog`, with the keys `deployment_id`,
`job`, `namespace`, `state`, `phase`, `hook_job` and `commit`.

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

Turn on one or more adapters in [configuration](configuration.md#notifications):

| Adapter | Sends |
|---|---|
| webhook | The JSON below, for n8n, Home Assistant, or your own receiver. |
| Discord | An embed, colored by state. |
| Slack | A text message. |
| ntfy | A plain-text message with a priority by state. |
| Gotify | A message with a priority by state. |

```json
{
  "deployment_id": "01J8ZX4N6Q9V3T2K5M7P8R1S0W",
  "job": "web",
  "namespace": "apps",
  "state": "failed",
  "error": "pre-hook web-migrate failed: exit code 1",
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "url": "https://nops.example.com/deployments/01J8ZX4N6Q9V3T2K5M7P8R1S0W",
  "time": "2026-09-23T10:00:00Z"
}
```

A notification waiting for a promotion also has `"waiting": "canary_promotion"`.

Nops tries each notification once, with no retry. A failed delivery logs a
WARN and never stops a deployment. The log never contains the adapter URL,
because Discord and Slack URLs hold a token.
