# Deployment lifecycle

A deployment is one attempt to bring a job to the spec in git. Nops tracks
it as a state machine in SQLite.

```
detected ──(policy=auto)──────────────┐
   │                                  ▼
   └─(policy=approval)→ pending_approval ──approve──→ [pre_hook] → applying → [post_hook] → completed
                             │                          │            │            │
                           reject                     fail/timeout  fail        fail/timeout
                             ▼                          ▼            ▼            ▼
                          rejected                    failed       failed       failed
```

- Policy `none` creates no deployment.
- Nops skips the states in `[ ]` when the job has no hooks of that phase.
- Pre-hooks run after approval, so a backup is fresh and a migration waits
  for a person's go-ahead.
- A failed post-hook doesn't undo the apply: the new version is already live.
- `completed`, `failed`, `rejected` and `superseded` are terminal.

## Supersede

A newer spec in git replaces a deployment still in `detected` or
`pending_approval`: the old one becomes `superseded`. A deployment already
applying finishes first.

## Revalidating pending deployments

Every detection cycle rechecks each waiting deployment. Nops supersedes it if
the policy became `none`, the spec in git changed, or someone changed the
live job. It completes it if the job is already in sync. Then, if there is
still drift, Nops creates a fresh deployment. A deployment waiting for
approval is always approvable, so there is no expiry.

## Not retrying an unchanged failure

After a deployment fails or is rejected, Nops doesn't create the same
deployment again while nothing changed: the job is **blocked**. A new commit
unblocks it. So does a person's **retry** from the dashboard: the next
deployment then follows the policy as usual, and can fail and block again.
If the live job already matches git, a retry only waits for it to be healthy
and runs the post-hooks again.

A deployment that failed after its register stays blocked even if the live
job changes, so Nomad's `auto_revert` can't cause an apply and revert loop.

## Holding a job

A **hold** keeps Nops from starting a deployment for a job. It comes from a
person's **pause** or from a closed [sync window](policies.md#sync-window).
While a job is held:

- Nops still shows the drift but creates no deployment;
- a deployment not started yet is superseded;
- a pause blocks approvals, and a sync window doesn't;
- a deployment already running finishes.

When the hold ends, Nops plans again from git. A blocked job stays blocked
while held.

## Schema

The migrations in `internal/store/migrations/` define the tables:

| Table | Holds |
|---|---|
| `deployments` | One row per deployment: job, commit, spec, redacted diff, policy, state, decisions. |
| `deployment_hooks` | The hooks each deployment froze when it was created. |
| `hook_runs` | One run of one hook of one deployment. |
| `job_pauses` | Pauses and resumes. Each records who did it and why. |
| `events` | The audit log of every deployment. |

A partial unique index on `deployments` allows one active deployment per job.
Every state change goes through one store function, which writes the event
in the same transaction.

## Recovery after a crash

On startup, the first apply cycle resumes every unfinished deployment from
what SQLite holds:

- `detected` and `pending_approval` continue as usual.
- `pre_hook` and `post_hook` wait for the hook run already dispatched, or
  dispatch it if Nomad never got it. If Nops can't tell whether a dispatch
  reached Nomad and finds no run, the hook fails rather than run twice.
- `applying` registers again only if the job's index is still the one read at
  detection. If the register already went through, Nops carries on.

Timeouts count from when each step started, not from the restart.
