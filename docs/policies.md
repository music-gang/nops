# Policies

A job's policy says what Nops does when it finds a difference between the repo
and the cluster. It is declared with `nops_policy` (see
[meta-keys](meta-keys.md)).

| Value | Behaviour |
|---|---|
| `auto` | Nops applies on its own: plan → (pre-hook) → CAS register → (post-hook). |
| `approval` | A `pending_approval` deployment is created with the diff; a human OK from the [dashboard](dashboard.md) is required. **Never** auto-applied. |
| `none` | Drift is shown but never applied, and no deployment is created. This is the default. |

An invalid meta key leads to `none` for the whole job (conservative reading).

## Which policy for which job

| Kind of job | Typical policy |
|---|---|
| Stateless service (frontend, API, proxy) | `auto` |
| Stateful service / with a volume / database | `approval` |
| Delicate or hand-managed jobs | `none` (observation only) |
| Batch / periodic | `auto`, or `approval` if the change touches data |

## Effect on apply

Apply is always the same, whatever the policy: a CAS register preceded by a
plan (see [architecture](architecture.md#apply-and-downtime)). The policy only
decides *who* gives the go-ahead, not *how* it is applied.

## Sync windows

Under `auto` Nops deploys whenever a change lands, at any hour: an image bump
merged by a bot at 3 a.m. is deployed at 3 a.m. A job can say **when** Nops may
deploy it on its own, and stay automatic the rest of the time, with two meta keys
([meta-keys](meta-keys.md)):

```hcl
nops_sync_window          = "0 9 * * 1-5"  # opens Mondays to Fridays at 09:00 ...
nops_sync_window_duration = "9h"           # ... and stays open until 18:00
```

- The expression is a cron for when the window **opens**, with the syntax of
  Nomad's `periodic` (the same library, `cronexpr`): 5 fields (minute, hour,
  day of month, month, day of week, with names such as `MON-FRI`), a sixth
  for the year, a seventh in front for the seconds, or a shorthand such as
  `@daily` (`TestSyncWindowAcceptsWhatPeriodicAccepts`). The detection cycle
  looks at it every `-drift-interval`, so seconds buy nothing. The duration
  says how long it stays open, the end exclusive. It is read in the instance's time zone,
  [`-sync-window-time-zone`](configuration.md#storage-intervals-and-timeouts)
  (UTC by default). There is one window per job, and only an *allow* one: "never
  between 00:00 and 07:00" is a window that opens at 07:00 for 17 hours.
- The job's page shows where the window stands (open until, or closed until),
  the rule, and the zone it is read in, so a window written in local time and
  read in UTC is seen at once.
- **Outside the window** drift is still detected and shown, and the job is
  **Held** ("outside its sync window, next opens Fri 2026-09-25 09:00 UTC"), but
  **no deployment is created**. When the window opens, detection plans again as
  usual, so what is applied is never a plan hours old, and the state machine
  gains no state ([state machine](state-machine.md#holding-a-job)).
- It **gates the start only**: a deployment already running is never
  interrupted when the window closes, and a pre-hook counts as the start.
- It gates **what Nops starts on its own**, self-heal included. Under
  `approval` the window is ignored, with a warning: the human OK is already the
  gate, and an approval given outside the window applies at once.
- A window is read from git like every other key: an invalid one is an error and
  policy `none` ([meta-keys](meta-keys.md#validation)). A person's
  [pause](#pausing-a-job) holds a job whatever its window says.

## Pausing a job

Under `auto` Nops puts a job back to what git says on its next cycle, which is
not what you want during an incident (a fix made by hand in Nomad, a
`nomad job revert`). **Pause** the job from its page on the
[dashboard](dashboard.md#pages), with a reason if you like:

- the drift is still detected and shown, and the job shows as **Paused**,
  whether or not it drifts, so a forgotten pause is seen; no deployment is
  created;
- a deployment waiting for approval stays, but cannot be approved until the
  job is resumed (it can still be rejected);
- a deployment already running (a pre-hook, the apply, a post-hook) finishes;
- **Resume** lifts the pause, and the next detection cycle plans again from
  git, so what is applied is never a plan from before the pause.

A pause holds a job under `auto` and under `approval` alike. Any logged-in
user can pause and resume, and both are logged with who did it. There is no
pause for the whole instance: stopping Nops does that. How a hold works inside
the engine is in [state machine](state-machine.md#holding-a-job).

## Approval

- Approve or reject from the deployment's page on the
  [dashboard](dashboard.md#pages): it shows the difference with the cluster
  and what *Approve* will do.
- Approving or rejecting is an authenticated human action, recorded in
  `events` with the actor and in `decided_by`/`decided_at`.
- An approval is valid for `(deployment_id, spec_hash)`. If the spec changes,
  the old pending deployment becomes `superseded` and a new one is created to
  approve.
- If the live job changes outside Nops, the pending deployment is
  revalidated automatically (see
  [state machine](state-machine.md#revalidating-pending-deployments)).
