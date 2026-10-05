# Policies

A job's policy says who gives the go-ahead when git and the cluster differ.
Set it with `nops_policy` ([meta keys](meta-keys.md)).

| Policy | What Nops does |
|---|---|
| `auto` | Deploys on its own. |
| `approval` | Creates a deployment and waits for a person to approve it on the [dashboard](dashboard.md). Never deploys on its own. |
| `none` | Shows the drift and deploys nothing. The default. |

The policy only decides who says go. Every deployment then runs the same way
([architecture](architecture.md#apply)).

## Which policy for which job

| Job | Policy |
|---|---|
| Stateless service (frontend, API, proxy) | `auto` |
| Stateful service, database, job with a volume | `approval` |
| Job you manage by hand | `none` |
| Batch or periodic | `auto`, or `approval` if it touches data |

## Sync window

A sync window limits when an `auto` job deploys on its own, for example only
during working hours:

```hcl
nops_sync_window          = "0 9 * * 1-5" # opens weekdays at 09:00
nops_sync_window_duration = "9h"          # and stays open until 18:00
```

- The cron expression says when the window opens, with the syntax of Nomad's
  `periodic`. The duration says how long it stays open.
- Nops reads windows in [`-sync-window-time-zone`](configuration.md#storage-intervals-and-timeouts),
  UTC by default.
- Outside the window, Nops still shows the drift but starts no deployment: the
  job is **Held**. When the window opens, Nops plans again from git.
- **Deploy now**, on the job's dashboard page, deploys the drift shown there
  once, like any deployment, without waiting for the window. If git changes
  first, the job stays held. It doesn't lift a pause or a block.
- A deployment that already started finishes even if the window closes.
- Under `approval`, Nops ignores the window: the approval is the gate.

## Pausing a job

Pause a job from its dashboard page to stop Nops from undoing a manual fix
during an incident. While a job is paused:

- Nops shows the drift but starts no deployment;
- you can't approve its pending deployment, but you can reject it;
- a deployment already running finishes.

**Resume** releases the job, and Nops plans again from git. Any logged-in
user can pause and resume, from the dashboard or the [API](api.md), and Nops
records who did it.

## Approval

Approve or reject a deployment from its dashboard page, which shows the diff
and what approving does. An approval covers that exact diff: if git or the
live job changes first, Nops replaces the deployment with a new one to
approve ([revalidation](deployment-lifecycle.md#revalidating-pending-deployments)).
