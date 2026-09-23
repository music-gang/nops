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

On every detection cycle, for each `pending_approval`:

- if the live `JobModifyIndex` differs from `cas_index`, the pending one moves
  to `superseded` (reason: "job modified outside nops") and, if there is still
  drift, a new deployment is created with an updated plan and index;
- if the plan is empty, the pending one moves to `completed` as a no-op
  (reason: "already in sync").

This way a pending deployment shown in the dashboard is always approvable.
There is no time-based expiry of pending deployments: this revalidation is
enough.

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
  `dispatched_job_id` is known, go back to waiting for it. Otherwise dispatch
  again with the same idempotency token (Nomad deduplicates). If the child no
  longer exists, the run is `failed` (outcome unknown), never dispatched again.
  The timeout is counted from `started_at`, not from the restart.
- `applying`: re-read the live job. If `JobModifyIndex == cas_index`, repeat
  the CAS register. If the index has changed and the plan of our spec is empty,
  the apply had already happened and we move on. Otherwise `failed` (conflict).
  To record `applied_index`, re-read the live job: the index in the register
  response is not reliable (see the decision log).

The idempotency token is scoped to the parent job and lives as long as the
child: if Nomad garbage-collects the child, a new dispatch becomes possible.
For this reason recovery always prefers the `dispatched_job_id` saved in
`hook_runs`, and a run whose child has vanished is `failed` rather than
dispatched again. Hook run states: `dispatching` (row created, child not saved
yet), `running` (child saved), then `succeeded`, `failed` or `timed_out`.
