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
  design questions are still open, so write the plan and the questions in a
  draft PR (docs only) and stop until the maintainer answers.
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
| `config` | Flags and `NOPS_*` env vars with validation ([configuration](configuration.md)) | `internal/config` |

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`redact`](#redact) | none | yes |
| [`notify`](#notify) | `config` | yes |
| [`gitwatch`](#gitwatch) | `config` | plan first |
| [`engine-detection`](#engine-detection) | `gitwatch`, `redact`, `notify` | plan first |
| [`engine-apply`](#engine-apply) | `engine-detection` | plan first |
| [`engine-recovery`](#engine-recovery) | `engine-apply` | plan first |
| [`web`](#web) | `engine-detection`, `config` | plan first |
| [`wiring`](#wiring) | `config`, `notify`, `gitwatch`, `engine-recovery`, `web` | yes |
| [`acceptance`](#acceptance) | `wiring` | yes |

### redact

Diff redaction, before anything is saved or shown.

- **Read first:** [dashboard.md](dashboard.md#secret-redaction), the patterns
  table in [philosophy.md](philosophy.md#patterns-reused-from-nomad-gitops).
- **Scope:** a pure function from the plan's `JobDiff` to the redacted JSON that
  goes into `plan_diff`. Redacts `Env[...]`, templates and keys containing
  password, token or secret. Suggested home: a small `internal/redact` package
  (add it to the layout in CLAUDE.md and to [architecture.md](architecture.md)).
- **Done when:** table-driven tests show that no secret value reaches the output
  for every pattern above, and that non-secret fields are untouched.
- **Notes:** open question for `engine-detection`: `deployments.job_spec` keeps
  the full spec to register, which can contain the same secrets, and it is not
  redacted. Either accept it (the database is local, and the spec is needed to
  apply) or re-read the spec from git at apply time and check `spec_hash`.
  Decide it in the `engine-detection` plan and write it in the decision log.

### notify

Notifications through a generic webhook.

- **Read first:** [error-handling.md](error-handling.md#notifications).
- **Scope:** `internal/notify`. A JSON POST to a configurable URL when a
  deployment becomes `pending_approval` and when it becomes `failed`. A
  configurable request timeout, no retries. A failed delivery is logged at
  WARN and never blocks the state machine.
- **Done when:** `httptest` tests cover the payload, a non-2xx answer, a
  timeout and an unreachable URL (all WARN, no error to the caller).
- **Notes:** the payload should carry `deployment_id`, `job`, `namespace`,
  `state`, `error` and `commit`, so ntfy, Gotify or n8n can use it without
  looking anything up. The URL and the request timeout come from
  `config.Config` (`NotifyURL`, `NotifyTimeout`); an empty URL means
  notifications are disabled, which is not an error.

### gitwatch

The repo, read-only, kept in memory.

- **Read first:** [architecture.md](architecture.md#execution-model-active-not-lazy),
  the patterns table in [philosophy.md](philosophy.md#patterns-reused-from-nomad-gitops)
  (go-git in memory, poll, webhook with a coalescing trigger), and
  [meta-keys.md](meta-keys.md#syntax-and-parsing) (`<job>.vars.hcl`).
- **Scope:** `internal/gitwatch`. Clone and poll with go-git, a trigger for the
  webhook (a channel of size 1, so bursts coalesce), and a way for the engine
  to get the job files (content, optional vars file) with the commit SHA they
  come from. Read-only: invariant 5.
- **Ready: plan first.** Questions for the plan: the interface the engine
  consumes; how a job file is told from a hook file and from a vars file; how
  the hook job is found by its ID; how to test against a local bare
  repository.
- **Notes from `config`:** credentials are settled: HTTPS basic auth with
  `config.Config.GitUsername` and `GitToken` (already read from its file; empty
  means a public repo), no SSH. Branch and poll interval are `GitBranch` and
  `GitPollInterval`.

### engine-detection

Compare the repo with Nomad and create the deployments.

- **Read first:** [state-machine.md](state-machine.md) (supersede, revalidating
  pending deployments, schema), [policies.md](policies.md),
  [hooks.md](hooks.md#contract), [error-handling.md](error-handling.md),
  [philosophy.md](philosophy.md) invariants 1, 4, 6, 7.
- **Scope:** `internal/engine`, detection only. For each managed job: parse
  (`nomadx.ParseHCL`, with the vars file), `meta.Parse`, plan, and if there is a
  difference create a deployment (`cas_index` = live `JobModifyIndex`, 0 if the
  job does not exist; redacted diff; spec hash). Supersede, revalidate pending
  deployments on every cycle, sync the hook jobs (register or update with plan +
  CAS, whatever the policy), notify on `pending_approval`. Not in scope: apply.
- **Ready: plan first.** Questions for the plan: the `job_spec` question in
  [`redact`](#redact); where the drift of a job under policy `none` is kept, as
  the schema has no table for observations (compute it on demand in the
  dashboard, or keep it in memory); a hook declared but missing from the repo
  is `failed`, never skipped.

### engine-apply

Move a deployment from approval to `completed`.

- **Read first:** [state-machine.md](state-machine.md),
  [architecture.md](architecture.md#apply-and-downtime),
  [hooks.md](hooks.md), [error-handling.md](error-handling.md),
  invariants 1, 2, 3, 7 in [philosophy.md](philosophy.md).
- **Scope:** the loop that advances non-terminal deployments: `pre_hook` →
  `applying` → `post_hook` → `completed`, and every failure path. Apply is a
  fresh plan (no difference: `completed` as a no-op) and `RegisterCAS` with the
  saved `cas_index`; `applied_index` comes from re-reading the live job, not
  from the register response. Then wait for the Nomad deployment to be
  `successful` (or the allocations to be running, if there is none), with a
  configurable timeout.
- **Ready: plan first.**
- **Notes from `hooks`:** `hooks.Runner.Run` blocks until the hook run is
  terminal, so run it in a goroutine per deployment. A returned error is a
  Nomad or SQLite failure: retry, do not fail the deployment. A `Result` with
  `failed` or `timed_out` fails the deployment: a pre-hook leaves the live job
  untouched, a post-hook does not undo the apply. `Request` needs the commit and
  the target job (parsed spec). The timeout is counted from `started_at`, so a
  restart does not extend it. A run that is already terminal returns its stored
  result without touching Nomad. Every transition goes through `Store.Transition`.

### engine-recovery

Resume what a restart interrupted.

- **Read first:** [state-machine.md](state-machine.md#recovery-after-a-crash).
- **Scope:** at startup, for every non-terminal deployment: `pre_hook` and
  `post_hook` call the hook runner again (it resumes by itself); `applying`
  re-reads the live job and decides between repeating the CAS register, moving
  on, or `failed` (conflict). `detected` and `pending_approval` need nothing.
- **Ready: plan first.**
- **Done when:** a table-driven test per case, including a crash between
  `Dispatch` and the saved child ID and a CAS conflict after restart.

### web

The dashboard and the git webhook.

- **Read first:** [dashboard.md](dashboard.md), [policies.md](policies.md#approval),
  [error-handling.md](error-handling.md).
- **Scope:** `internal/web`. Pending deployments with the redacted diff and
  approve/reject, history, auth from the proxy header (403 on a write without
  it), POST only with a CSRF token, the git webhook endpoint that fires the
  `gitwatch` trigger. Approve and reject only record the human decision with
  `Store.Transition` (`decided_by`); the engine does the rest.
- **Ready: plan first.** Question for the plan: how the webhook is
  authenticated (a shared secret in the request).
- **Done when:** `httptest` tests for the handlers, header auth, CSRF and 403.
  There is no coverage target. Complete [dashboard.md](dashboard.md), which is
  still marked "to be completed".

### wiring

- **Read first:** [architecture.md](architecture.md), the config docs.
- **Scope:** `cmd/nops`: build config, store, Nomad client, engine and web,
  start the three loops, shut down cleanly on a signal.
- **Notes from `config`:** `config.Load(os.Args[1:], os.Getenv, os.Stderr)`
  (exit 0 on `flag.ErrHelp`); the Nomad client is
  `nomadx.New(cfg.Nomad(), cfg.NomadNamespace)`, never `api.DefaultConfig()`;
  `hooks.New` takes `HookPollInterval`; log a WARN at startup when
  `NomadTLSSkipVerify` is set. Never log the `Config` itself: it holds the
  tokens.
- **Done when:** `go build ./...` produces the binary and it starts against a
  `nomad agent -dev` (a smoke test in `tests/integration`).

### acceptance

- **Read first:** [development.md](development.md#integration) (manual
  checklists for the real cluster).
- **Scope:** `docs/acceptance/`: one checklist per realistic scenario (heavy
  image pre-pull on a host volume, backup before a stateful deploy, approval
  flow). The README status note changes from "early development" to usable.
- **Done when:** the checklists exist and were run once against the real cluster.
