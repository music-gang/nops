# State machine and store

Every deployment goes through a state machine persisted in SQLite.

```
detected ──(policy=auto)──────────────┐
   │                                  ▼
   └─(policy=approval)→ pending_approval ──approve──→ [pre_hook] → applying → [post_hook] → completed
                             │                          │            │            │
                           reject                     fail/timeout  fail        fail/timeout
                             ▼                          ▼            ▼            ▼
                          rejected                    failed       failed       failed
```

- `none` creates no deployment: drift is only an observation in the dashboard.
- States in `[ ]` are skipped when the hook is not defined.
- The `pre_hook` runs **after** approval: migrations and backups must not start
  before the human OK, and the backup ends up fresh.
- A post-hook failure does not undo the apply (the job is already live):
  `failed` with an explicit message and a notification, no automatic rollback.
- Terminal states: `completed`, `failed`, `rejected`, `superseded`. They have
  no outgoing transitions.

## Supersede

A newer commit on the same job moves a deployment that is still in `detected`
or `pending_approval` to `superseded`. If the deployment is already in
`pre_hook`/`applying`/`post_hook`, nothing is created and the next cycle
retries.

## Revalidating pending deployments

On every detection cycle, for each `detected` or `pending_approval`
deployment (in that order; see [engine-detection](design/engine-detection.md)
for the full per-job procedure):

- if the job's policy is now `none`, it moves to `superseded` (reason:
  "policy changed to none");
- else if the target spec changed, it moves to `superseded` (reason: "newer
  spec at commit `<sha>`") — this compares `spec_hash`, never `commit_sha`: an
  unrelated commit that does not touch the job's file must not disturb it;
- else if the live `JobModifyIndex` differs from `cas_index`, it moves to
  `superseded` (reason: "job modified outside nops");
- else if the plan is empty, it moves to `completed` as a no-op (reason:
  "already in sync").

In every `superseded` case, a new deployment is created right after if there
is still drift and the job's policy allows one (see
[policies](policies.md)); creation follows the normal rules, including a
retry check below.

This way a pending deployment shown in the dashboard is always approvable.
There is no time-based expiry of pending deployments: this revalidation is
enough.

## Not retrying an unchanged failure

A new deployment is not created for a job whose most recent one is `failed`
or `rejected` with the same `spec_hash` **and** the same live index
(`cas_index`): nothing that produced that outcome has changed, so recreating
it would only repeat the same failure, the same event and the same
notification on every cycle. A new commit, or a change to the live job
(including nops's own apply, once it exists), makes a new deployment again.

## Schema

The detail lives in `internal/store/migrations/`; this is the summary.

- `deployments`: id (ULID), job_id, namespace, commit_sha, spec_hash,
  job_spec (JSON), plan_diff (redacted JSON), policy, state, cas_index,
  applied_index, eval_id, error, decided_by, decided_at, created_at,
  updated_at.
  `UNIQUE INDEX (namespace, job_id) WHERE state IN (detected,
  pending_approval, pre_hook, applying, post_hook)` is the per-job lock.
- `hook_runs`: id, deployment_id, phase, hook_job_id, idempotency_token
  (`<deployment_id>:<phase>`), dispatched_job_id, state
  (`dispatching|running|succeeded|failed|timed_out`), timeout_s, error,
  started_at, finished_at. `UNIQUE(deployment_id, phase)`.
- `events`: append-only log (deployment_id, ts, from_state, to_state, actor,
  message). It feeds the history view in the dashboard.

Store rules:

- `Store.Transition` is the only way to change `state`: it validates the
  transition table, requires the starting state (compare-and-set on the state)
  and writes the event in the same transaction.
- A single SQLite connection: writes are serialized.

## Recovery after a crash

At startup, for every non-terminal deployment:

- `pre_hook`/`post_hook`: run the hook again (`hooks.Runner.Run` is
  idempotent). A run that is already terminal returns its stored result. If
  `dispatched_job_id` is known, go back to waiting for the dispatched job. If the run is
  `dispatching`, nothing was sent yet and it is dispatched. If it is `running`
  without a dispatched job ID, the dispatch may have been sent: look the dispatched job up by its
  idempotency token and adopt it; if there is none, the run is `failed` (outcome
  unknown), never dispatched again. A dispatched job that no longer exists is `failed`
  too. The timeout is counted from `started_at`, not from the restart.
- `applying`: re-read the live job. If `JobModifyIndex == cas_index`, repeat
  the CAS register. If the index has changed and the plan of our spec is empty,
  the apply had already happened and we move on. Otherwise `failed` (conflict).
  To record `applied_index`, re-read the live job: the index in the register
  response is not reliable (see the decision log).

The idempotency token is scoped to the parent job and lives as long as the
dispatched job: if Nomad garbage-collects the dispatched job, a new dispatch becomes possible.
For this reason recovery always prefers the `dispatched_job_id` saved in
`hook_runs`, and a run whose dispatched job has vanished is `failed` rather than
dispatched again. Hook run states: `dispatching` (row created, nothing sent to
Nomad), `running` (saved before the dispatch is sent; the dispatched job ID follows), then
`succeeded`, `failed` or `timed_out`.
