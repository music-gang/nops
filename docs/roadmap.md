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
| `config` | Flags and `NOPS_*` env vars with validation ([configuration](configuration.md)) | #8, `internal/config` |
| `redact` | Secret values removed from the plan diff ([rules](dashboard.md#secret-redaction)) | #9, `internal/redact` |
| `notify` | Notifications through built-in adapters ([notifications](error-handling.md#notifications)) | `internal/notify` |
| `gitwatch` | In-memory git watcher ([design](design/gitwatch.md)), `-git-path` in `config` | #12, `internal/gitwatch` |

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`engine-detection`](#engine-detection) | `gitwatch`, `redact`, `notify` | plan first |
| [`engine-apply`](#engine-apply) | `engine-detection` | plan first |
| [`engine-recovery`](#engine-recovery) | `engine-apply` | plan first |
| [`web`](#web) | `engine-detection`, `config` | plan first |
| [`wiring`](#wiring) | `config`, `notify`, `gitwatch`, `engine-recovery`, `web` | yes |
| [`acceptance`](#acceptance) | `wiring` | yes |

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
- **Ready: plan first.** Questions for the plan: `deployments.job_spec` keeps
  the full spec to register, which can contain the same secrets as the diff,
  and it is not redacted: either accept it (the database is local, and the
  spec is needed to apply) or re-read the spec from git at apply time and
  check `spec_hash`, then write the choice in the decision log; where the
  drift of a job under policy `none` is kept, as the schema has no table for
  observations (compute it on demand in the dashboard, or keep it in memory);
  a hook declared but missing from the repo is `failed`, never skipped.
- **Notes from `redact`:** `redact.Diff(plan.Diff)` returns the JSON for
  `Deployment.PlanDiff` (as a string); it never modifies the plan, so the same
  `JobDiff` can still drive the decision. An error from it is a bug, not a
  Nomad failure: fail the cycle loudly, never save the unredacted diff.
- **Notes from `notify`:** call `Notifier.Notify(ctx, d)` with the deployment
  as saved, right after `Store.Transition` to `pending_approval` or `failed`
  succeeds, never before (invariant 7) and never for other states. It never
  returns an error. It is synchronous and can take up to `-notify-timeout` per
  adapter, so call it in a goroutine if the cycle must not wait. Declare a
  one-method interface for it in `engine`.
- **Notes from `gitwatch`:** `Watcher.Snapshot()` gives `{Commit, Files}`;
  each `File` has `Path`, `Content`, and `VarsPath`/`Vars` (empty when there
  is no vars file). Parse every file with `nomadx.ParseHCL(ctx, f.Content,
  f.Vars)`, then classify by `meta.Parse`'s result: `nops_role == "hook"` is
  a hook, `nops_managed` is a managed job, anything else is ignored. Two
  files parsing to the same job ID are both ignored, with an ERROR. Run
  detection on `Watcher.Changed()` and on the drift ticker; a parse cache
  keyed by `(Content, Vars)` avoids re-parsing unchanged files on every drift
  tick.

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
- **Notes from `notify`:** call `Notifier.Notify(ctx, d)` with the deployment
  as saved, right after `Store.Transition` to `failed` succeeds, never before
  (invariant 7); `pending_approval` is detection's. It never returns an error.
  It is synchronous and can take up to `-notify-timeout` per adapter, so call
  it in a goroutine if the cycle must not wait. Declare a one-method interface
  for it in `engine`.

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
- **Notes from `notify`:** notifications link to
  `<PublicURL>/deployments/<id>` (`config.Config.PublicURL`, no trailing
  slash): the dashboard must serve the deployment page at that path.
- **Notes from `gitwatch`:** the webhook handler calls `Watcher.Trigger()`
  (non-blocking) after checking the shared secret; it does not wait for a
  poll to happen.

### wiring

- **Read first:** [architecture.md](architecture.md), the config docs.
- **Scope:** `cmd/nops`: build config, store, Nomad client, engine and web,
  start the three loops, shut down cleanly on a signal.
- **Notes from `config`:** `config.Load(os.Args[1:], os.Getenv, os.Stderr)`
  (exit 0 on `flag.ErrHelp`); the Nomad client is
  `nomadx.New(cfg.Nomad(), cfg.NomadNamespace)`, never `api.DefaultConfig()`;
  `hooks.New` takes `HookPollInterval`; log a WARN at startup when
  `NomadTLSSkipVerify` is set. Never log the `Config` itself: it holds the
  tokens and the notification URLs.
- **Notes from `notify`:** `notify.New(notify.Options{...}, log)` built from
  the `config.Config` notification fields (`NotifyWebhookURL` /
  `NotifyWebhookToken`, `NotifyDiscordURL`, `NotifySlackURL`, `NotifyNtfyURL` /
  `NotifyNtfyToken`, `NotifyGotifyURL` / `NotifyGotifyToken`, `PublicURL`,
  `NotifyTimeout`). With no adapter set it is a no-op, not an error.
- **Notes from `gitwatch`:** `gitwatch.New(gitwatch.Options{URL: cfg.GitURL,
  Branch: cfg.GitBranch, Path: cfg.GitPath, Username: cfg.GitUsername,
  Token: cfg.GitToken, PollInterval: cfg.GitPollInterval}, log)`; call
  `Start` before serving (its error is fatal, there is nothing to run
  detection on), then run `Run` in its own goroutine.
- **Done when:** `go build ./...` produces the binary and it starts against a
  `nomad agent -dev` (a smoke test in `tests/integration`).

### acceptance

- **Read first:** [development.md](development.md#integration) (manual
  checklists for the real cluster).
- **Scope:** `docs/acceptance/`: one checklist per realistic scenario (heavy
  image pre-pull on a host volume, backup before a stateful deploy, approval
  flow). The README status note changes from "early development" to usable.
- **Done when:** the checklists exist and were run once against the real cluster.
