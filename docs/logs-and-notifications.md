# Logs and notifications

What Nops writes to its log, and when it tells people through a notification.
Why it prefers to stop and say so rather than carry on with uncertain state is
in [philosophy](philosophy.md#fail-loud).

## What is always logged

Every state transition is logged at INFO. **Every `failed`** is logged at
ERROR with `deployment_id`, `job`, `phase` (where it was when it failed:
`detection`, `pre`, `apply` or `post`) and the cause (`error`), and sends a
[notification](#notifications). `pending_approval` sends a notification too,
and so does an `applying` deployment that starts waiting for a canary
promotion.

Structured `log/slog`, with the standard keys `deployment_id`, `job`,
`namespace`, `state`, `phase`, `hook_job`, `commit`.
A line carries each key once: the JSON handler (the default) writes a repeated
key twice, and a parser that rejects duplicates would drop the line.

| Event | Level |
|---|---|
| State transition | INFO |
| An `applying` deployment starts waiting for a canary promotion, or sees the canaries promoted | INFO |
| Canaries promoted from the dashboard (`Engine.Promote`, with the `actor`) | INFO |
| A job paused or resumed from the dashboard (`Engine.Pause`, `Engine.Resume`, with the `actor` and, for a pause, the reason) | INFO |
| Nomad refuses to promote, or a read for the dashboard's Nomad panel fails (once per cached panel) | ERROR |
| Unknown meta key | WARN |
| Notification not delivered (also counted in [metrics](metrics.md)) | WARN |
| Deployment `failed`, meta with an invalid value, job that cannot be parsed | ERROR |
| Nomad or SQLite error | ERROR (with context) |

## Notifications

Nops tells people when a deployment needs them: it sends a notification when
a deployment becomes `pending_approval` (someone must approve) and when it
becomes `failed`, and once when an `applying` deployment starts waiting for a
person to promote the canaries of its Nomad deployment (a
[promotion wait](glossary.md#deployment-lifecycle): its `state` is `applying`
and `waiting` is `canary_promotion`; the title is `<job> waiting for canary
promotion`, styled like `pending_approval`). A job can also opt in to a notification when a deployment
of it becomes `completed` (`nops_notify_completed`, [meta-keys](meta-keys.md));
unlike the other two this one is never sent by default, since a cluster with
many `auto` jobs would otherwise get one per deploy. The engine calls
[`internal/notify`](../internal/notify/notify.go) after the transition has been
saved, never before.

Notifications go through built-in **adapters**, each with its own options
(see [configuration](configuration.md#notifications)). An adapter is on when
its URL is set, and several can be on at once.

| Adapter | What it sends |
|---|---|
| webhook | The generic JSON below, `Content-Type: application/json`, with `Authorization: Bearer` if there is a token. For n8n, Home Assistant or your own receiver. |
| Discord | An embed: title `<job> waiting for approval` / `<job> failed` / `<job> completed`, the error as description, namespace and commit as fields, the link as the title URL. Red for `failed`, amber for `pending_approval`, green for `completed`. Texts are cut to Discord's limits. |
| Slack | A `text` message in mrkdwn: title, namespace, commit, the error as a quote, `<link\|Open in nops>`. |
| ntfy | A plain-text body, with `Title`, `Priority` (4 for `failed`, 3 for `pending_approval`, 2 for `completed`: it needs no action, so it must not compete for attention), `Tags`, and `Click` (the link) headers, and `Authorization: Bearer` if there is a token. |
| Gotify | `{title, message, priority}` (8 for `failed`, 5 for `pending_approval`, 2 for `completed`), the link as `extras.client::notification.click.url`, and the app token in `X-Gotify-Key`. |

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

`waiting` is only present, as `canary_promotion`, in the notification of a
promotion wait. `url` is `<public-url>/deployments/<id>`; `-public-url` always has a value —
explicit, or [derived from `-listen-addr`](configuration.md#dashboard) — so a
notification always has one. The notifier itself still tolerates an empty one
and then sends no link.

A notification is the only soft failure: each adapter gets one attempt,
bounded by `-notify-timeout`, with no retry. An answer that is not 2xx, a
timeout, or an unreachable server is logged at WARN (`adapter`,
`deployment_id`, `job`, `namespace`, `state` and the cause). It never
reaches the engine, and the other adapters still get the notification. The
log never contains the URL, because a Discord or Slack URL holds a token.
