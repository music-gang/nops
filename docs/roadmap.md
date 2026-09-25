# Roadmap

What is done, what is in flight and what is left. It is written so that a
session that only has this repo (a cloud session, a phone request like "do
`config`") can pick a task and take it to a pull request without other context.
The working steps are in [CLAUDE.md](../CLAUDE.md#picking-up-work).

## How to read it

- A task is `todo` or `done`. **In flight is not written here**: it is a remote
  branch whose name ends with the task ID (`feat/config` works on `config`).
  `git ls-remote --heads origin` lists them.
- A task is startable when every task in *Depends on* is `done` and no branch
  for it exists.
- **Ready** says whether it can be done unattended. `yes`: go. `plan first`:
  design questions are still open, so lay out the plan and the questions to
  the maintainer in the session and wait for their answers — no branch, no PR
  for this step.
- The PR that finishes a task sets it to `done` here and adds the decisions it
  took to the [decision log](design/decisions.md). The *Notes* of the tasks that
  come next are updated with what was learned.
- To add a task: a new block with a unique lowercase-hyphen ID (it becomes a
  branch name), and the same fields as the others.

## Done

| Task | What | Where |
|---|---|---|
| `meta` | Parsing and validation of the `nops_*` meta keys | `internal/meta` |
| `store` | SQLite store, migrations, state machine transitions | `internal/store` |
| `nomadx` | Nomad client wrapper, CAS-only register | `internal/nomadx` |
| `ci-setup` | GitHub Actions, repo policies, license | #1, #2 |
| `hooks` | Hook runner: dispatch, wait, timeout, stop, recovery of a run | #3, `internal/hooks` |
| `vocabulary` | [Vocabulary](vocabulary.md) of the terms used everywhere | #4 |
| `pr-template` | No PR template: the description is the commit body | #5, #6 |
| `config` | Flags and `NOPS_*` env vars with validation ([configuration](configuration.md)) | #8, `internal/config` |
| `redact` | Secret values removed from the plan diff ([rules](dashboard.md#secret-redaction)) | #9, `internal/redact` |
| `notify` | Notifications through built-in adapters ([notifications](error-handling.md#notifications)) | `internal/notify` |
| `gitwatch` | In-memory git watcher ([design](design/gitwatch.md)), `-git-path` in `config` | #12, `internal/gitwatch` |
| `engine-detection` | Detection cycle: parse, plan, create/supersede/revalidate deployments, hook sync ([design](design/engine-detection.md)) | `internal/engine` |
| `engine-apply` | Apply loop: approve/reject, register, health, timeout, the anti-loop rule and blocked drift ([design](design/engine-apply.md)) | `internal/engine` |
| `engine-recovery` | Recovery after a restart: `RunApply`'s first cycle, crash-window tests, health fails on an outside edit ([design](design/engine-apply.md#decisions), 8) | #16, `internal/engine` |
| `web-auth` | OIDC login of the dashboard: allowlist, session, `Require`, cross-origin protection ([dashboard](dashboard.md#authentication)); `-auth-header` replaced by the `-oidc-*` options | `internal/web`, `internal/config` |
| `web` | Dashboard pages (pending, history, drift, deployment detail with an htmx-polled status fragment), diff renderer, git webhook per forge ([dashboard](dashboard.md)) | `internal/web`, `internal/config` |
| `local-auth` | A second login backend, local users (`-auth-mode=basic`, `username:bcrypt-hash` file), mutually exclusive with OIDC (`-auth-mode=oidc`) via a required `-auth-mode`; session/cookie/`Require`/logout shared between the two through `web.Authenticator` ([dashboard](dashboard.md#authentication)) | `internal/web`, `internal/config` |
| `wiring` | `cmd/nops`: builds every component, runs the three loops and the dashboard, shuts down cleanly on `SIGINT`/`SIGTERM`; a `run(ctx, cfg, log) error` factored out of `main` for the whole wiring to be one reviewable, testable unit ([binary smoke test](development.md#integration)) | `cmd/nops` |
| `dashboard-data` | The backend the redesigned dashboard needs: commit subject/author on deployments and `gitwatch.CommitURL`, `gitwatch.Status` and `Engine.Status`, `Store.ListByJob`/`LatestPerJob`, **retry** of a blocked job (`Engine.Retry`, [engine-apply](design/engine-apply.md), 9) and `POST /fetch`, with their endpoints and tests; no page shows them yet | `internal/gitwatch`, `internal/store`, `internal/engine`, `internal/web` |
| `dashboard-ux` | The redesigned dashboard: Overview (what needs attention, git and cycle health), Jobs (sync state), Job, Deployment (a review page: what Approve will do, the diff summarized and folded once decided) and Activity, in a dense Primer look ([dashboard](dashboard.md), [decisions](design/decisions.md) 2026-09-24) | `internal/web`, `internal/engine` |
| `e2e` | Simulated end-to-end integration tests in place of manual acceptance checklists: approval flow, self-heal under `auto`, the pre-hook scenarios (pre-pull, backup) with `raw_exec`, and `examples/` kept parsing ([development](development.md#integration)); the real check is using nops on the cluster ([first rollout](development.md#first-rollout-on-a-real-cluster)) | `tests/integration` |

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`dashboard-polish`](#dashboard-polish) | `dashboard-ux` | yes |
| [`hook-revisions`](#hook-revisions) | — | yes |
| [`multi-hooks`](#multi-hooks) | `hook-revisions` | yes |
| [`orphan-jobs`](#orphan-jobs) | — | yes |

What else remains is using nops on a real cluster; what that turns up becomes
new tasks here.

### dashboard-polish

Fix what using the redesigned dashboard on the real cluster turns up: layout,
wording, empty states. No new features; anything bigger becomes its own task.

- **Read first:** [dashboard.md](dashboard.md), the decision log
  (2026-09-24, the `dashboard-ux` rows).
- **Ideas already noted, not promised:** a count of what needs attention in
  the nav (it costs three queries per page, so not done in `dashboard-ux`);
  a *Retry* on the Activity rows; the diff of a job that does not exist yet
  shows every field as added, which is true and long (a folded, per-group
  view would read better); a link from the Overview's git strip to the
  history of that commit.
- **Done when:** the maintainer has used it for real and the list they gave is
  closed.

### hook-revisions

Make the approval cover the hooks that will run, and stop two deployments that
share a hook from racing on one Nomad job.

- **Read first:** [hooks.md](hooks.md), [engine-detection.md](design/engine-detection.md),
  [engine-apply.md](design/engine-apply.md), the decision log (2026-09-25,
  "Hook revisions").
- **The problem:** `spec_hash` covers only the target job. The hook that runs
  is whatever is registered in Nomad under its fixed ID when it is dispatched,
  that is the latest in git, not what was there when the deployment was
  approved; and between one deployment's register and its dispatch another
  can register a different version (Nomad has no CAS on dispatch).
- **Decided** (with the maintainer, 2026-09-25):
  - At detection the deployment freezes, for every hook it declares, the hook
    ID, the hash of its spec and the spec itself, taken from the same
    snapshot as the target. The deployment's `spec_hash` becomes the hash of
    the target and its hooks, in order: approving covers what will run.
  - A hook revision is registered in Nomad as `<hook-id>-<first 8 hex of its
    hash>`, keeping `Name` as the hook's own ID, **right before it is
    dispatched**, from the frozen spec: `Plan` + `RegisterCAS` at index 0,
    skipped when the plan shows no change (the revision is already there).
    Nothing is registered before approval, and the per-cycle hook sync
    (`syncHooks`) goes.
  - GC after every detection cycle: deregister, **without purge**, every job
    with `nops_role = "hook"` and an ID matching `^(.+)-[0-9a-f]{8}$` that no
    non-terminal deployment uses. A failure is an ERROR, retried next cycle.
  - No migration for hooks already registered under their fixed ID: nops is not
    in use yet.
- **First step:** check on `nomad agent -dev` 2.0.3, and write in the decision
  log, that deregistering a `parameterized` parent leaves its dispatched jobs
  and their logs readable, that a job whose `Name` differs from its `ID`
  registers, and that the suffixed ID hits no length limit.
- **Scope:**
  - `internal/store`: migration `0003` with `deployment_hooks(deployment_id,
    phase, position, hook_id, revision, spec_hash, job_spec)`, written by
    `CreateDeployment` in the same transaction; `hook_runs.hook_job_id` holds
    the revision. `position` is for `multi-hooks`: always 0 here.
  - `internal/engine/detect.go`: build the frozen hooks from what `classify`
    returns, compute the combined hash (reuse `specHash`), use it in
    revalidation and `blockedRetry`; remove `syncHooks`, keep `missingHook`.
  - `internal/engine/apply.go` (`stepHook`): read the hook from
    `deployment_hooks`, register the revision, then `hooks.Run` with the
    revision as `HookJobID`. `internal/hooks` does not change.
  - `internal/nomadx`: list jobs by ID prefix, deregister without purge.
  - `internal/web`: `planSteps` reads `deployment_hooks` instead of parsing the
    target's meta; the hook rows show the hook and a short revision.
  - Docs: `hooks.md`, `engine-detection.md`, `engine-apply.md`,
    `state-machine.md` (schema), `architecture.md` ("does not deregister jobs"
    gets the hook-revision exception), `vocabulary.md` (**hook revision**),
    decision log.
- **Done when:** unit tests for the combined hash, the revision ID, a changed
  hook superseding a pending deployment and unblocking a blocked one, the GC's
  selection and the register at dispatch; the migration tested; integration and
  e2e against Nomad showing nothing registered before approval, the revision
  registered and dispatched after it, a hook changed while pending superseding
  the deployment, and an unused revision deregistered (the `TestE2EPreHook*`
  tests adapted).
- **Consequences to keep:** a hook changed in git supersedes a pending
  deployment (it must be approved again), a fixed hook unblocks a blocked job
  on its own, and a hook change alone never creates a deployment for a target
  with no drift.

### multi-hooks

More than one pre-hook and post-hook per job (for example, backup then
migrate).

- **Read first:** [meta-keys.md](meta-keys.md), [hooks.md](hooks.md),
  `hook-revisions` above, the decision log (2026-09-25, "Multiple hooks").
- **Decided** (with the maintainer, 2026-09-25):
  - `nops_pre_hook` / `nops_post_hook` take a comma-separated list; one hook is
    written as today.
  - The hooks of a phase run in order, one after the other; the first failure
    stops the phase (a pre-hook failure leaves the live job untouched, a
    post-hook failure fails the deployment with the job already live, as
    today). No parallel runs.
  - The timeout moves onto the hook job: `nops_timeout` in its meta (default
    5m), valid only with `nops_role = "hook"`, frozen with the revision.
    `nops_pre_hook_timeout` and `nops_post_hook_timeout` are removed.
  - The same hook twice in one phase is a meta error (policy `none` + ERROR).
- **Scope:**
  - `internal/meta` (source of truth, with `meta-keys.md`): `PreHooks` /
    `PostHooks []Hook` in place of `PreHook` / `PostHook`, list parsing (trim,
    no empty item, no duplicate), `nops_timeout`, the two old keys become
    unknown.
  - `internal/store`: migration adding `position` to `hook_runs`,
    `UNIQUE(deployment_id, phase, position)`, idempotency token
    `<deployment_id>:<phase>:<n>`.
  - `internal/engine`: `stepHook` walks the phase's positions in order; a
    finished run returns its stored result, so recovery resumes at the first
    unfinished one; `nextAfterDecision` uses the length of the list. The state
    machine does not change.
  - `internal/web`: every hook of the plan in order; hook rows sorted by phase
    and position.
  - `examples/`: a scenario with two pre-hooks (`TestExamplesParse` covers it).
  - Docs: `meta-keys.md`, `hooks.md`, `engine-apply.md`, `state-machine.md`,
    decision log.
- **Done when:** table tests for the meta (list, duplicates, `nops_timeout`,
  removed keys), engine tests for order, stop at the first failure and recovery
  in the middle of the list, the migration, and an e2e where two pre-hooks run
  in order and a failing first one keeps the second from running.

### orphan-jobs

Tell the operator when a job nops deployed is no longer in the repository but
still runs in Nomad.

- **Read first:** [dashboard.md](dashboard.md), [engine-detection.md](design/engine-detection.md),
  the decision log (2026-09-25, "Orphan jobs").
- **Decided** (with the maintainer, 2026-09-25): **alert only**, like Argo CD's
  default "OutOfSync, requires pruning". nops never stops or deregisters an
  orphan: the operator stops it in Nomad or puts the file back, and the alert
  clears on its own. This builds the observation a later "Stop" action would
  use; that action, and a guard against mass removals, are a separate task if
  ever needed.
- **An orphan** is a job that nops has deployed (a `completed` deployment),
  that is not among the jobs *parsed* from the snapshot whatever their
  classification (a job still in the repository without `nops_managed` is
  **not** an orphan: that means "hands off"), and that exists in Nomad with
  `Stop != true` (stopped or purged: not an orphan). In a cycle where any file
  fails to parse, the orphan check is suspended: a broken file looks exactly
  like a removed one.
- **Scope:**
  - `internal/store`: the jobs with at least one `completed` deployment in the
    namespace.
  - `internal/engine`: in `Detect`, the IDs of every parsed job; for each
    deployed job not among them, `nomad.Job`; `Engine.Orphans()` in memory,
    rebuilt every cycle like `Observations()`; `Status` gains `Orphans` and
    `OrphanCheckSkipped`. A Nomad error on one candidate is an ERROR and skips
    it.
  - `internal/web`: sync state **Not in git** on Jobs (rows for orphans, which
    have no observation), a Needs attention row "Removed from git, still
    running in Nomad", a banner on the job page with what to do (`nomad job
    stop <id>`, or restore the file), and the suspended check on the Overview.
  - Docs: `vocabulary.md` (**orphan**), `dashboard.md`, `engine-detection.md`,
    decision log.
- **Done when:** engine tests with the fake Nomad (found; not when still parsed
  but unmanaged; not when stopped; not when purged; not when never completed;
  suspended with an unparsed file), page tests, and an e2e: deploy a job, remove
  its file, see it; `nomad job stop` it, see it gone.
