# Errors and failures: fail loud

nops prefers to stop and say so rather than carry on with uncertain state.

## Rules

- **Every state transition goes through a single store function**
  (`Store.Transition`), which updates `deployments` and writes `events` in the
  same transaction and logs at INFO. There are no "manual" transitions.
- **Every `failed`** is logged at ERROR with `deployment_id`, `job`, `phase`
  and the cause, and sends a webhook notification. `pending_approval` sends a
  notification too.
- **Never swallow an error** from Nomad or SQLite. The only "soft" exception is
  the notification: if it fails, log at WARN and do not block the state
  machine.
- **If a SQLite write fails, the reconciler stops** for that cycle: we do not
  act on Nomad with state that has not been persisted.
- **When in doubt, take the conservative path:**
  - invalid meta key → policy `none` for the whole job;
  - hook declared but not found → `failed` (never "proceed without the hook");
  - CAS conflict → `failed` + re-detection.
- Messages in `error` and `events` must be useful to whoever looks at the
  dashboard: what failed, where, and what to do next.

## What is always logged

Structured `log/slog`, with the standard keys `deployment_id`, `job`,
`namespace`, `state`, `phase`, `hook_job`, `commit`.

| Event | Level |
|---|---|
| State transition | INFO |
| Unknown meta key | WARN |
| Notification not delivered | WARN |
| Deployment `failed`, meta with an invalid value, job that cannot be parsed | ERROR |
| Nomad or SQLite error | ERROR (with context) |

## Notifications

A generic webhook: a JSON POST to a configurable URL (works with ntfy, Gotify,
n8n, or Slack/Discord through an adapter). Notifications are sent on
`pending_approval` and on `failed`.
