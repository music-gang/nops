# Vocabulary

The words nops uses, so that code, docs, commits, PRs and conversation say the
same thing. When a word here and a word somewhere else disagree, this page wins
and the other place gets fixed. When a new concept appears, or a term is
renamed, add or change its row **in the same PR**.

Identifiers in code follow the term: `HookRun`, `DispatchedJobID`,
`cas_index`. Discussion in another language keeps these terms untranslated
(*deployment*, *hook*, *supersede*), so they stay searchable.

## Nomad's words and ours

Some words exist in both worlds and mean different things. Say which one.

| Word | In nops | In Nomad | Rule |
|---|---|---|---|
| **deployment** | One attempt to bring a job to a given spec, a row in SQLite with a state machine. | The rolling-update tracker Nomad creates after a register. | Bare "deployment" is always the nops one. Write **Nomad deployment** for the other. |
| **job** | A Nomad job, seen through nops. | The same. | Qualify it when it matters: **live job**, **target job**, **hook job**, **dispatched job**. |
| **evaluation**, **allocation** | Not used as nops concepts. | Scheduler terms. | Only in code that talks to Nomad (`nomadx`, outcome detection). |
| **stop** | Deregistering a dispatched hook job without purge (`StopJob`). | Same operation, `DELETE /v1/job/<id>`. | nops stops only dispatched jobs of hooks that timed out. It never deregisters anything else. |

## Jobs and specs

| Term | Meaning |
|---|---|
| **live job** | The job as it is in Nomad right now. |
| **spec** | The HCL of a job as parsed by Nomad. The **target spec** is the one in the repo that a deployment wants to reach. |
| **spec hash** | Hash of the target spec (`spec_hash`). An approval is valid for one `(deployment_id, spec_hash)`. |
| **target job** | The job a deployment is about (the hook receives it as `nops_job_id`). |
| **managed job** | A target job with `nops_managed = "true"`, eligible for deployment detection. A target job without it is ignored by detection; hook jobs are separate and use `nops_role = "hook"`. |
| **meta key** | A `nops_*` key in a job's `meta` block: the [only syntax](meta-keys.md) nops reads. |
| **policy** | `auto`, `approval` or `none`: who gives the go-ahead. It says nothing about *how* the apply is done. |
| **drift** | A difference between the target spec and the live job. Under policy `none` it is only shown. |
| **plan** | Nomad's dry-run of a register (`Jobs.Plan`), with the diff. |
| **diff** | The plan's `JobDiff`. Once redacted, it is the **redacted diff** saved in SQLite and shown in the dashboard ([rules](dashboard.md#secret-redaction)). |

## Deployment lifecycle

| Term | Meaning |
|---|---|
| **detection** | Comparing the repo with Nomad and creating, superseding or revalidating deployments. One pass is a **detection cycle**. |
| **deployment** | See above. States: `detected`, `pending_approval`, `pre_hook`, `applying`, `post_hook`, `completed`, `failed`, `rejected`, `superseded`. |
| **state** | Where a deployment (or a hook run) is in its state machine. Not the same as *phase*. |
| **active deployment** | One in a non-terminal state. A job has at most one: the **per-job lock**, enforced by the partial unique index. |
| **terminal state** | `completed`, `failed`, `rejected`, `superseded`. No way out. |
| **pending deployment** | A deployment in `pending_approval`. |
| **approve / reject** | The authenticated human decision on a pending deployment, recorded with the **actor**. |
| **actor** | Who did something: the logged-in user (`preferred_username`, else `email`, else `sub` of the OIDC token) for a decision, `nops` for the engine. It goes in `decided_by` and `events.actor`. |
| **session** | The signed, encrypted cookie set after an OIDC login, valid 12 hours, with a key that lives only in the process ([dashboard](dashboard.md#authentication)). |
| **allowlist** | `-oidc-allowed-users` and `-oidc-allowed-groups`: who may log in. Required. |
| **supersede** | A newer commit (or a change outside nops) replaces a deployment that has not started applying. Result: `superseded`. |
| **revalidation** | Re-checking every pending deployment on each detection cycle, so the one shown in the dashboard is always approvable. |
| **observation** | The last detection cycle's drift for one managed job (policy, whether it drifted, its redacted diff), kept only in memory (`Engine.Observations()`), never in SQLite: it is always a function of the current git head and the current live job, so there is nothing to persist. It is how a `none` policy job's drift reaches the dashboard, since no deployment is ever created for it. |
| **blocked drift** | Drift a retry rule is deliberately not turning into a new deployment (`Observation.BlockedBy`/`BlockedReason`): either the existing rule for a `failed`/`rejected` deployment with an unchanged `spec_hash` and live index, or the anti-loop rule for a deployment that `failed` after reaching the register. Only a new commit unblocks it. |
| **apply** | The CAS register of the target spec, always preceded by a plan. Nomad carries out the update. |
| **CAS** | Compare-and-set on the job's modify index. |
| **cas index** | The live `JobModifyIndex` captured at detection (`cas_index`). `0` means "the job must not exist". |
| **applied index** | The live `JobModifyIndex` re-read after the apply (`applied_index`), never the one in the register response. |
| **recovery** | Resuming non-terminal deployments after a restart: it is the first cycle of the engine loop, not a separate pass. Every step is written to be **resumable**: calling it again continues where it stopped. |
| **event** | A row of the append-only audit log, written together with every transition. The **actor** is a user name or `nops`. |
| **fail loud** | Never swallow a Nomad or SQLite error; a failure is logged, stored and notified. |
| **conservative reading** | When in doubt take the safe path: an invalid meta key means policy `none`, a missing hook means `failed`. |
| **notification** | The message nops sends when a deployment becomes `pending_approval` or `failed`. Not delivered is a WARN, never an error. See [notifications](error-handling.md#notifications). |
| **notification adapter** | One way to deliver a notification: `webhook` (generic JSON), `discord`, `slack`, `ntfy`, `gotify`. Each has its own options and is on when its URL is set. |

## Hooks

| Term | Meaning |
|---|---|
| **hook** | The concept: a job nops runs before (`pre`) or after (`post`) an apply. |
| **phase** | `pre` or `post`. It is the value of `nops_phase`. The deployment *states* around it are `pre_hook` and `post_hook`. |
| **hook job** | The `batch` + `parameterized` job in the repo, marked `nops_role = "hook"`. Inert until dispatched. Also called the parent in Nomad's API. |
| **hook sync** | nops registering or updating the hook job (plan + CAS) before dispatching it. |
| **dispatch** | Asking Nomad to start a run of the hook job (`Jobs.Dispatch`). |
| **dispatched job** | The job Nomad creates from a dispatch (`<hook job>/dispatch-<id>`), stored as `dispatched_job_id`. Nomad's API calls it a child (`ParentID`); code may say `child` for the parent/child link, docs say *dispatched job*. |
| **dispatch meta** | The `nops_*` values passed at dispatch: `nops_deployment_id`, `nops_job_id`, `nops_commit`, `nops_phase`, `nops_image_<task>`. Only the ones the hook declares. |
| **idempotency token** | `<deployment_id>:<phase>`. Makes a second dispatch return the same dispatched job. It lives as long as that job. |
| **hook run** | The execution of one hook for one deployment and phase: a row in `hook_runs`, unique per `(deployment_id, phase)`. Run by `hooks.Runner`. |
| **hook run states** | `dispatching` (row created, nothing sent), `running` (saved *before* the dispatch is sent), then `succeeded`, `failed`, `timed_out`. |
| **outcome** | What the dispatched job and its allocations say about a run: success, failure, stopped from outside, or not decided yet. |
| **timeout** | The configured duration (`nops_*_hook_timeout`), in whole seconds. |
| **deadline** | `started_at + timeout`. It does not move when nops restarts. |
| **outcome unknown** | A run that may have been dispatched but whose dispatched job cannot be found. It is `failed` and never dispatched again. |

## Code and tooling

| Term | Meaning |
|---|---|
| **store** | `internal/store`: SQLite, the only place state lives. |
| **nomadx** | `internal/nomadx`: the Nomad client wrapper, CAS-only register, sentinel errors. |
| **engine** | `internal/engine`: the state machine that moves deployments forward. Its two loops are **detection** and the **engine loop** (advancing non-terminal deployments); **recovery** is the engine loop's first cycle. |
| **runner** | `hooks.Runner`: runs one hook run to a terminal state. Blocking and idempotent. |
| **sentinel error** | An exported `Err...` value the caller tests with `errors.Is` (`ErrCASConflict`, `ErrJobNotFound`, `ErrActiveDeployment`). |
| **fake / stub** | *Fake*: an in-memory stand-in with behaviour (the fake Nomad in `hooks` tests). *Stub*: an `httptest` server that returns canned answers (`nomadx` tests). |
| **integration test** | A test against a real `nomad agent -dev` (build tag `integration`). |

## Work on the repo

| Term | Meaning |
|---|---|
| **PR** | Pull request. Its **title** is the commit that lands on `main`, in Angular style `type(scope): subject`. |
| **roadmap** | [`roadmap.md`](roadmap.md): the tasks left, with dependencies, what to read and when each is done. |
| **task** | One block of the roadmap. Its ID is the suffix of the branch that works on it. |
| **in flight** | A task with a remote branch (or an open PR). Derived from git, never written in the roadmap. |
| **ready / plan first** | Whether a task can be done unattended, or needs the plan and its open questions reviewed in a draft PR first. |
| **decision log** | [`design/decisions.md`](design/decisions.md): one row per design decision, added in the PR that takes it. |
| **invariant** | One of the seven rules in [philosophy](philosophy.md) that no change may break. |
