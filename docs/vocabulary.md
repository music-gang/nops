# Vocabulary

The words Nops uses, so that code, docs, commits, PRs and conversation say the
same thing. When a word here and a word somewhere else disagree, this page wins
and the other place gets fixed. When a new concept appears, or a term is
renamed, add or change its row **in the same PR**.

Identifiers in code follow the term: `HookRun`, `DispatchedJobID`,
`cas_index`. Discussion in another language keeps these terms untranslated
(*deployment*, *hook*, *supersede*), so they stay searchable.

## Nomad's words and ours

Some words exist in both worlds and mean different things. Say which one.

| Word | In Nops | In Nomad | Rule |
|---|---|---|---|
| **deployment** | One attempt to bring a job to a given spec, a row in SQLite with a state machine. | The rolling-update tracker Nomad creates after a register. | Bare "deployment" is always the Nops one. Write **Nomad deployment** for the other. |
| **job** | A Nomad job, seen through Nops. | The same. | Qualify it when it matters: **live job**, **target job**, **hook job**, **dispatched job**. |
| **evaluation**, **allocation** | Not used as Nops concepts. | Scheduler terms. | Only in code that talks to Nomad (`nomadx`, outcome detection). |
| **version** | The build of Nops: a release tag (`v0.1.0`) or, for a dev build, a Go pseudo-version ([releasing](development.md#releasing)). | A job's version (`Job.Version`), which Nomad bumps on a register that changes the job. | Write **Nops version** or **job version** when both could be meant. |
| **stop** | Deregistering a job without purge (`StopJob`): a dispatched job whose hook timed out, or a hook revision no deployment needs. | Same operation, `DELETE /v1/job/<id>`. | Nops stops a dispatched job of a hook that timed out, and the hook revisions no deployment needs. It never deregisters anything else. |

## Jobs and specs

| Term | Meaning |
|---|---|
| **live job** | The job as it is in Nomad right now. |
| **spec** | The HCL of a job as parsed by Nomad. The **target spec** is the one in the repo that a deployment wants to reach. |
| **spec hash** | Hash that identifies what a deployment will do (`spec_hash`): the target spec combined with the frozen hooks, in order (the target's own when it has none). An approval is valid for one `(deployment_id, spec_hash)`. |
| **target job** | The job a deployment is about (the hook receives it as `nops_job_id`). |
| **managed namespace** | A namespace listed in `-nomad-namespaces`: the only ones Nops reads jobs for, deploys to and looks for orphans in. A job is identified by `(namespace, ID)`; a hook lives in its job's namespace ([configuration](configuration.md#namespaces)). |
| **managed job** | A target job with `nops_managed = "true"`, eligible for deployment detection. A target job without it is ignored by detection; hook jobs are separate and use `nops_role = "hook"`. |
| **meta key** | A `nops_*` key in a job's `meta` block: the [only syntax](meta-keys.md) Nops reads. |
| **policy** | `auto`, `approval` or `none`: who gives the go-ahead. It says nothing about *how* the apply is done. |
| **drift** | A difference between the target spec and the live job. Under policy `none` it is only shown. |
| **plan** | Nomad's dry-run of a register (`Jobs.Plan`), with the diff. |
| **diff** | The plan's `JobDiff`. Once redacted, it is the **redacted diff** saved in SQLite and shown in the dashboard ([rules](dashboard.md#secret-redaction)). |

## Deployment lifecycle

| Term | Meaning |
|---|---|
| **detection** | Comparing the repo with Nomad and creating, superseding or revalidating deployments. One pass is a **detection cycle**; the loop that runs it is also called the **reconciler**. |
| **deployment** | See above. States: `detected`, `pending_approval`, `pre_hook`, `applying`, `post_hook`, `completed`, `failed`, `rejected`, `superseded`. |
| **state** | Where a deployment (or a hook run) is in its state machine. Not the same as *phase*. |
| **active deployment** | One in a non-terminal state. A job has at most one: the **per-job lock**, enforced by the partial unique index. |
| **terminal state** | `completed`, `failed`, `rejected`, `superseded`. No way out. |
| **pending deployment** | A deployment in `pending_approval`. |
| **approve / reject** | The authenticated human decision on a pending deployment, recorded with the **actor**. |
| **actor** | Who did something: the logged-in user for a decision, `nops` for the engine. The user is the OIDC token's `preferred_username`, else `email`, else `sub` (`-auth-mode=oidc`), or the username as is (`-auth-mode=basic`). It goes in `decided_by` and `events.actor`. |
| **session** | The signed, encrypted cookie set after a login, either backend (`-auth-mode`), valid 12 hours, with a key that lives only in the process ([dashboard](dashboard.md#authentication)). |
| **allowlist** | `-oidc-allowed-users` and `-oidc-allowed-groups`: who may log in. Required. |
| **supersede** | A newer commit (or a change outside Nops) replaces a deployment that has not started applying. Result: `superseded`. |
| **revalidation** | Re-checking every pending deployment on each detection cycle, so the one shown in the dashboard is always approvable. |
| **observation** | The last detection cycle's drift for one managed job (policy, whether it drifted, its redacted diff), kept only in memory (`Engine.Observations()`), never in SQLite: it is always a function of the current git head and the current live job, so there is nothing to persist. It is how a `none` policy job's drift reaches the dashboard, since no deployment is ever created for it. |
| **blocked drift** | Drift a retry rule is deliberately not turning into a new deployment (`Observation.BlockedBy`/`BlockedReason`): either the existing rule for a `failed`/`rejected` deployment with an unchanged `spec_hash` and live index, or the anti-loop rule for a deployment that `failed` after reaching the register. Only a new commit or a **retry** unblocks it. |
| **retry** | A human lifting a **blocked drift** without a new commit (`Engine.Retry`): the blocking deployment is marked *retried* (`retried_by`, `retried_at`) and stops blocking, and the next detection cycle creates a new deployment that follows the policy. It never approves and never applies anything by itself. |
| **hold** | A condition that keeps Nops from *starting* a deployment for a job (`Observation.Hold`, `engine.Hold`): detection still plans it and shows the drift, but creates nothing, and an `auto` deployment still `detected` is superseded. It gates the start only (a deployment in `pre_hook`/`applying`/`post_hook` finishes) and only ever subtracts. It has two sources: a person's **pause** and a closed **sync window** ([state-machine](state-machine.md#holding-a-job)). Not a **blocked drift**: that is about a failed deployment and leaves the job's policy to a *retry*. |
| **sync window** | When Nops may deploy a job **on its own** (`nops_sync_window` + `nops_sync_window_duration`, read in `-sync-window-time-zone`): a scheduled **hold**, outside of which the job is **Held** (sync state) and no deployment is created. Only under policy `auto`; it gates the start, never a deployment already running ([policies](policies.md#sync-windows)). |
| **pause / resume** | A person holding a job (`Engine.Pause`) and lifting it (`Engine.Resume`), from the Job page or the Overview: who, when and an optional reason are kept in SQLite (`job_pauses`), the sync state is **Paused**, and Approve is refused while it lasts. It is not *frozen*: that word is taken by the **frozen hook**. |
| **sync state** | Where a managed job stands against git, in one word, as the Jobs page shows and filters it: invalid meta, paused, blocked, not in git, awaiting approval, deploying, held, drift or in sync (the first that applies wins; [dashboard](dashboard.md#sync-state-of-a-job)). Not a deployment state: it is about the *job*. |
| **needs attention** | What the Overview lists for a person to act on: approvals waiting, blocked jobs, failures nobody retried, meta errors, paused jobs, orphans. Not drift under policy `none`: that is a sync state. |
| **orphan** | A job Nops deployed (it has a `completed` deployment) that is no longer in the repository but still exists in Nomad, not stopped and not `dead`. Reported (`Engine.Orphans()`, sync state **Not in git**, a line in **needs attention**), never stopped by Nops ([engine-detection](design/engine-detection.md#orphan-jobs)). A job still in the repository without `nops_managed` is not one. |
| **apply** | The CAS register of the target spec, always preceded by a plan. Nomad carries out the update. |
| **healthy** | What ends `applying` ([engine-apply](design/engine-apply.md#decisions), 3): the Nomad deployment that tracks the applied index is `successful`; when there is none (a `batch` job, or no `update` block), every allocation of the applied job version is `running` or `complete`. That is a weaker test than a health check: Nops does not compare the number of allocations with the counts. A job that never has an allocation of its own (periodic, parameterized, or every group at count 0) is healthy as soon as it is registered. A Nomad deployment that waits for a canary promotion is not healthy yet, and not failing either: see **promotion wait**. |
| **promotion wait** | What an `applying` deployment does while the job's Nomad deployment is running with its canaries placed and healthy and not all of them promoted yet, and its `update` block has `auto_promote = false`: it waits for a person to promote them in Nomad. The apply timeout does not run in it, it is notified once, and the Deployment page says so; the timeout counts again from the promotion ([engine-apply](design/engine-apply.md#decisions), 10). A person promotes from the Deployment page (**Promote**) or in Nomad ([decision 11](design/engine-apply.md#decisions)). |
| **Nomad panel** | The read-only box of the Job and Deployment pages with what Nomad reports about a job: its status, its groups against their counts, its latest Nomad deployment. Nothing computed by Nops on top ([dashboard](dashboard.md#the-nomad-panel)). |
| **CAS** | Compare-and-set on the job's modify index. |
| **cas index** | The live `JobModifyIndex` captured at detection (`cas_index`). `0` means "the job must not exist". |
| **applied index** | The live `JobModifyIndex` re-read after the apply (`applied_index`), never the one in the register response. |
| **recovery** | Resuming non-terminal deployments after a restart: it is the first cycle of the **apply loop**, not a separate pass. Every step is written to be **resumable**: calling it again continues where it stopped. |
| **event** | A row of the append-only audit log, written together with every transition. The **actor** is a user name or `nops`. |
| **fail loud** | Never swallow a Nomad or SQLite error; a failure is logged, stored and notified. |
| **conservative reading** | When in doubt take the safe path: an invalid meta key means policy `none`, a missing hook means `failed`. |
| **notification** | The message Nops sends when a deployment needs a person. Which transitions send one is defined in [notifications](error-handling.md#notifications). Not delivered is a WARN, never an error. |
| **notification adapter** | One way to deliver a notification: `webhook` (generic JSON), `discord`, `slack`, `ntfy`, `gotify`. Each has its own options and is on when its URL is set. |

## Hooks

| Term | Meaning |
|---|---|
| **hook** | The concept: a job Nops runs before (`pre`) or after (`post`) an apply. |
| **phase** | `pre` or `post`. It is the value of `nops_phase`. The deployment *states* around it are `pre_hook` and `post_hook`. |
| **hook job** | The `batch` + `parameterized` job in the repo, marked `nops_role = "hook"`. Inert until dispatched. Also called the parent in Nomad's API. |
| **hook revision** | A hook job at one exact spec, registered in Nomad as `<hook-id>-<first 8 hex of its spec hash>` right before it is dispatched, from the spec the deployment froze. The unit that is dispatched, and the one [garbage collected](hooks.md#hook-revisions) when no deployment in progress needs it. |
| **frozen hook** | A hook as a deployment saw it when it was detected: ID, spec, hash and revision (`deployment_hooks`). Approving covers it. |
| **dispatch** | Asking Nomad to start a run of the hook job (`Jobs.Dispatch`). |
| **dispatched job** | The job Nomad creates from a dispatch (`<hook job>/dispatch-<id>`), stored as `dispatched_job_id`. Nomad's API calls it a child (`ParentID`); code may say `child` for the parent/child link, docs say *dispatched job*. |
| **dispatch meta** | The `nops_*` values passed at dispatch: `nops_deployment_id`, `nops_job_id`, `nops_commit`, `nops_phase`, `nops_image_<task>`. Only the ones the hook declares. |
| **idempotency token** | `<deployment_id>:<phase>:<position>`. Makes a second dispatch return the same dispatched job. It lives as long as that job. |
| **hook run** | The execution of one hook for one deployment: a row in `hook_runs`, unique per `(deployment_id, phase, position)`. Run by `hooks.Runner`. |
| **position** | The place of a hook among those of its phase, from 0, in the order they are listed in `nops_pre_hook` / `nops_post_hook`. The hooks of a phase run in position order and stop at the first failure. |
| **hook run states** | `dispatching` (row created, nothing sent), `running` (saved *before* the dispatch is sent), then `succeeded`, `failed`, `timed_out`. |
| **outcome** | What the dispatched job and its allocations say about a run: success, failure, stopped from outside, or not decided yet. |
| **timeout** | The duration a hook may run (`nops_timeout` on the hook job, frozen with its revision), in whole seconds. |
| **deadline** | `started_at + timeout`. It does not move when Nops restarts. |
| **outcome unknown** | A run that may have been dispatched but whose dispatched job cannot be found. It is `failed` and never dispatched again. |

## Code and tooling

| Term | Meaning |
|---|---|
| **Nops** / **`nops`** | **Nops** is the product, written with a capital in prose (docs, README, PR descriptions), like Nomad. **`nops`** in code font is the command: the binary, the image (`ghcr.io/music-gang/nops`), the Go module, a path (`cmd/nops`), a log or config name. `NOPS_*` variables and `nops_*` meta keys are keys and keep their spelling. The logo may be lowercase: its lettering is not the prose spelling. |
| **store** | `internal/store`: SQLite, the only place state lives. |
| **nomadx** | `internal/nomadx`: the Nomad client wrapper, CAS-only register, sentinel errors. |
| **engine** | `internal/engine`: the state machine that moves deployments forward. Its two loops are **detection** (also called the **reconciler**: parses, plans, creates/supersedes/revalidates deployments) and the **apply loop** (advances non-terminal deployments); **recovery** is the apply loop's first cycle. |
| **runner** | `hooks.Runner`: runs one hook run to a terminal state. Blocking and idempotent. |
| **sentinel error** | An exported `Err...` value the caller tests with `errors.Is` (`ErrCASConflict`, `ErrJobNotFound`, `ErrActiveDeployment`). |
| **fake / stub** | *Fake*: an in-memory stand-in with behaviour (the fake Nomad in `hooks` tests). *Stub*: an `httptest` server that returns canned answers (`nomadx` tests). |
| **integration test** | A test against a real `nomad agent -dev` (build tag `integration`). |

## Work on the repo

The mechanics behind these terms (branching, PRs, how an issue becomes a PR)
are in [CLAUDE.md](../CLAUDE.md#picking-up-work); this is just what to call
them.

| Term | Meaning |
|---|---|
| **PR** | Pull request. Its **title** is the commit that lands on `main`, in Angular style `type(scope): subject`. |
| **issue** | A GitHub issue: one piece of work, with its problem, its proposal and, while it is being designed, what is still to decide. The open issues are the work left. |
| **in flight** | An issue with a remote branch or an open PR. Derived from git and GitHub, never written down. |
| **plan first** | An issue whose design questions still need the maintainer's answer before any branch exists. |
| **changelog** | [`CHANGELOG.md`](../CHANGELOG.md): per release, what changed for someone running Nops; a PR adds its line under `Unreleased` ([changelog](development.md#changelog)). |
| **release PR** | The PR `chore(release): vX.Y.Z` that `scripts/release.sh` opens: it turns the `Unreleased` lines into the version's section, the notes the release publishes. |
| **decision log** | [`design/decisions.md`](design/decisions.md): the design decisions up to v0.4.0, kept as history and no longer written to. A decision's reason is now in the page that explains that part. |
| **invariant** | One of the seven rules in [philosophy](philosophy.md) that no change may break. |
| **doc audit** | Checking the claims of the docs against the code and its tests, before a release or on request; a report of findings, then fixes as agreed ([procedure](development.md#doc-audit)). Not the tests that already compare tables and test names with the code. |
| **release** | A `vX.Y.Z` tag on `main` and what the `release` workflow publishes for it: the image on GHCR, the binary, checksums and the version's section of the changelog on the GitHub Release ([releasing](development.md#releasing)). |
