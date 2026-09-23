# Errors and failures: fail loud

nops prefers to stop and say so rather than carry on with uncertain state.

## Rules

- **Every state transition goes through a single store function**
  (`Store.Transition`), which updates `deployments` and writes `events` in the
  same transaction and logs at INFO. There are no "manual" transitions.
- **Every `failed`** is logged at ERROR with `deployment_id`, `job`, `phase`
  and the cause, and sends a [notification](#notifications).
  `pending_approval` sends a notification too.
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

nops tells people when a deployment needs them: it sends a notification when
a deployment becomes `pending_approval` (someone must approve) and when it
becomes `failed`. The engine calls
[`internal/notify`](../internal/notify/notify.go) after the transition has been
saved, never before.

Notifications go through built-in **adapters**, each with its own options
(see [configuration](configuration.md#notifications)). An adapter is on when
its URL is set, and several can be on at once.

| Adapter | What it sends |
|---|---|
| webhook | The generic JSON below, `Content-Type: application/json`, with `Authorization: Bearer` if there is a token. For n8n, Home Assistant or your own receiver. |
| Discord | An embed: title `<job> waiting for approval` / `<job> failed`, the error as description, namespace and commit as fields, the link as the title URL. Red for `failed`, amber for `pending_approval`. Texts are cut to Discord's limits. |
| Slack | A `text` message in mrkdwn: title, namespace, commit, the error as a quote, `<link\|Open in nops>`. |
| ntfy | A plain-text body, with `Title`, `Priority` (4 for `failed`, 3 for `pending_approval`), `Tags`, and `Click` (the link) headers, and `Authorization: Bearer` if there is a token. |
| Gotify | `{title, message, priority}` (8 for `failed`, 5 for `pending_approval`), the link as `extras.client::notification.click.url`, and the app token in `X-Gotify-Key`. |

The generic JSON, which is also the data every adapter formats:

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

`url` is empty when `-public-url` is not set.

A notification is the only soft failure: each adapter gets one attempt,
bounded by `-notify-timeout`, with no retry. An answer that is not 2xx, a
timeout, or an unreachable server is logged at WARN (`adapter`,
`deployment_id`, `job`, `namespace`, `state` and the cause). It never
reaches the engine, and the other adapters still get the notification. The
log never contains the URL, because a Discord or Slack URL holds a token.
