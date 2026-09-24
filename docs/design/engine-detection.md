# engine-detection design

The detection half of `internal/engine` (the [`engine-detection`](../roadmap.md)
task): comparing the repository with Nomad, and creating, superseding or
revalidating deployments. It never applies anything — that is
[`engine-apply`](../roadmap.md#engine-apply).

## Scope

One detection cycle (`Engine.Detect`) runs once at startup, then again on
every `gitwatch.Watcher.Changed()` signal and on every `-drift-interval` tick
(`Engine.RunDetection`). A cycle:

1. Parses every file of the current `gitwatch.Snapshot` through Nomad
   (`nomadx.ParseHCL`), with a cache keyed by the hash of `(Content, Vars)` so
   an unchanged file is not re-sent on every drift tick.
2. Classifies each parsed job with `meta.Parse`: `nops_role = "hook"` is a
   hook, `nops_managed = "true"` is a managed job, anything else is ignored.
   Two files parsing to the same job ID are both ignored, with an ERROR (see
   [gitwatch](gitwatch.md#job-hook-or-neither-decided-by-content)). Every
   `meta.Issue` found is logged at its severity, whatever the file turns out
   to be.
3. Syncs every hook job (register or update with plan + CAS), whatever the
   policy of the job it serves.
4. For every managed job: plans it, revalidates or supersedes its existing
   deployment if there is one, and creates a new one if there is still drift
   to apply.
5. Supersedes the deployment of any managed job that left the repository
   entirely.

## Interfaces

`engine` declares its own small interfaces over the packages it consumes
(the pattern used everywhere else in nops, see
[architecture.md](../architecture.md)):

- `Nomad`: `ParseHCL`, `Job`, `Plan`, `RegisterCAS`. `*nomadx.Client` implements it.
- `Store`: `CreateDeployment`, `Transition`, `GetDeployment`,
  `ActiveDeployment`, `ListActive`, `LatestDeployment`. `*store.Store` implements it.
- `Snapshots`: `Snapshot`, `Changed`. `*gitwatch.Watcher` implements it.
- `Notifier`: `Notify`. `*notify.Notifier` implements it.

## Decisions

These are recorded in the [decision log](decisions.md); the reasoning is
expanded here.

### `job_spec` keeps the full, unredacted spec

`deployments.job_spec` holds the JSON of the parsed job nops will register at
apply time, exactly as read from git (after the scaling-count substitution
below). It is not redacted, so it can carry the same secrets as the plan
diff. This is accepted, not worked around: the database is local to the host
(same trust boundary as the HCL files and as Nomad's own copy of the job),
apply needs the exact spec that was planned and, for `approval`, the one a
human saw; re-reading it from git at apply time would not work in general,
since `gitwatch` only keeps the current head and a later, unrelated commit
can move past what a still-pending deployment needs.

Mitigation: the database file (and its `-wal`/`-shm` sidecars) are created and
kept at permission `0600` (`store.Open`), and `job_spec` is never rendered by
the dashboard (see [dashboard.md](../dashboard.md#secret-redaction)).

### Drift of a `none` policy job lives only in memory

`nops_policy = "none"` never creates a deployment (see
[policies.md](../policies.md)), but the dashboard still needs to show that
such a job has drifted, with its redacted diff. The schema has no table for
this, and it is not given one: `Engine.Observations()` returns the last
cycle's drift, one `Observation` per managed job (whatever its policy),
rebuilt from scratch on every cycle. A job that leaves the repository (or
starts failing to parse) simply disappears from the next call: there is
nothing to expire or garbage-collect. Recomputing it costs nothing extra,
since the plan was already run to decide whether to create a deployment.

Not chosen: computing it on demand when the dashboard page loads (a Nomad
round trip per job per page view, and it duplicates this cycle's logic), or a
new table (persistent, but there is nothing here that must survive a
restart: the next cycle, seconds later, recomputes it, and a table would need
its own row lifecycle — created, updated, expired — for a value that is
always a function of the current git head and the current live job).

### A hook declared but missing from the repo fails at detection

If `nops_pre_hook` or `nops_post_hook` names a job ID that is not a hook file
in the current snapshot, the deployment is created and immediately moved
`detected → failed`, with a notification, rather than waiting for approval
first. Asking a human to approve a deployment that is guaranteed to fail at
the hook step serves nobody; the fix (add the hook file, or fix its meta) is
the same either way. This is in addition to, not instead of, the runtime
check `hooks.Runner` already does at dispatch time (the hook job must also
still be registered, of type `batch`, and parameterized) — that one stays,
since git can change between detection and apply.

### Retry rule after a `failed` or `rejected` deployment

Without a rule, a job whose deployment keeps failing (a broken pre-hook, for
example) would get a brand new `failed` deployment, event and notification
every single cycle. A new deployment is not created when the job's latest
one (`Store.LatestDeployment`) is `failed` or `rejected` with the same
`spec_hash` **and** the same live index (`cas_index`): nothing that produced
the earlier outcome has changed. A new commit (different `spec_hash`) or a
change to the live job (a different index — including nops's own apply, once
`engine-apply` exists) makes a new deployment again, which matches invariant
2's "a CAS conflict leads to `failed` and a re-detection".

### `spec_hash` is computed before the live-cluster adjustment

A task group with a `scaling` block takes its `Count` from the *live* job
before the plan is run (see below), so nops does not fight the autoscaler
(see [philosophy](../philosophy.md#patterns-reused-from-nomad-gitops)).
`spec_hash` is
computed on the job exactly as parsed from git, before that substitution, so
the autoscaler moving a count never supersedes a pending approval or
triggers the retry rule to reset.

### Supersede is keyed on `spec_hash`, not on the commit

A commit that does not touch a job's file must not disturb its pending
deployment. Every "does the spec still hold" check compares `spec_hash`
(and, separately, the live index), never `commit_sha`: `CommitSHA` on the row
is informational (shown in the dashboard and in notifications), not part of
the identity of what was approved.

### Not covered by a rule (accepted, documented)

- **A job stopped by hand** (`nomad job stop`) plans as a difference from
  git. Under `auto` nops restarts it: git is the source of truth (invariant
  4). Use `approval` or `none` for a job you intend to stop by hand.
- **Hook sync happens independently of any one deployment's phase.** A hook
  updated by a newer commit is registered as soon as it is seen, even while
  an older deployment is still `pre_hook` or `post_hook` for the previous
  version. Hooks must already be idempotent and tolerate being dispatched
  again (see [hooks.md](../hooks.md#rules-for-hook-authors)); this adds "and
  possibly a newer version of themselves" to that requirement.
- **A Nomad parse failure on a file that used to parse** does not, by
  itself, supersede that job's pending deployment: the job simply does not
  appear in this cycle's "seen" set, which only matters once nothing at all
  in the snapshot still produces its job ID (see "job removed from
  repository" below). A transient or persistent parse error is logged at
  ERROR every cycle until the file is fixed.

## Per-job reconciliation

For each managed job, in order:

1. If there is an active deployment in `pre_hook`, `applying` or `post_hook`,
   it is left untouched: an apply in progress is `engine-apply`'s concern.
2. If there is one in `detected` or `pending_approval`:
   - policy is now `none` → `superseded` ("policy changed to none");
   - else `spec_hash` differs → `superseded` ("newer spec at commit `<sha>`");
   - else the live index differs from `cas_index` → `superseded` ("job
     modified outside nops");
   - else there is no more drift → `completed` ("already in sync");
   - else it is left as is (still approvable).
3. If the policy is `none`, or there is no drift, nothing more happens.
4. If the retry rule applies (see above), nothing more happens.
5. Otherwise a deployment is created in `detected`, with the redacted diff,
   the full spec and the live index as `cas_index`. If a declared hook is
   missing from the repo it is immediately moved to `failed` (with a
   notification); otherwise `approval` moves it to `pending_approval` (with a
   notification) and `auto` is left in `detected` for `engine-apply`.

Finally, every deployment still in `detected` or `pending_approval` whose job
is no longer seen anywhere in the snapshot is `superseded` ("job removed from
repository"): nops never deregisters a job on its own.

A store failure aborts the whole cycle (invariant 7: never act on Nomad with
unpersisted state); a `redact.Diff` failure aborts it too (never store an
unredacted diff). Every other failure — a bad file, a job Nomad cannot plan,
a hook that cannot be synced — is scoped to the one file or job it came from:
logged, and the cycle continues with the rest.

### The commit is recorded on the deployment; the cycle status lives in memory

A deployment stores the subject and author of its commit
(`commit_subject`, `commit_author`) next to `commit_sha`, taken from the
`gitwatch.Snapshot` the cycle read: the clone is shallow and only ever holds
the head, so a page about a deployment from three commits ago cannot ask git
what that commit said. `Engine.Status()` reports how the last cycle went (when
it ended, how long it took, the error that aborted it, how many managed jobs it
found, how many it could not plan because of a Nomad failure, how many files
Nomad could not parse), in memory like `Observations()` and for the same reason:
it is recomputed every cycle. It exists because a cycle that logs an ERROR for
every job and produces no observation looks, from the dashboard, exactly like a
cluster with nothing to do.

## Tests

`internal/engine`: a fake Nomad and a real, temp-file `store.Store`, table-
and scenario-driven over the cases in [Decisions](#decisions) and
[Per-job reconciliation](#per-job-reconciliation) above, plus the parse cache
and the observation map never growing across cycles. `tests/integration`:
one cycle against a real `nomad agent -dev` — real parse, plan and hook sync,
a redacted diff with the secret confirmed absent, and revalidation once the
live job changes outside nops.
