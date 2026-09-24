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
| `engine-detection` | Detection cycle: parse, plan, create/supersede/revalidate deployments, hook sync ([design](design/engine-detection.md)) | `internal/engine` |

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`engine-apply`](#engine-apply) | `engine-detection` | plan first |
| [`engine-recovery`](#engine-recovery) | `engine-apply` | plan first |
| [`web`](#web) | `engine-detection`, `config` | plan first |
| [`wiring`](#wiring) | `config`, `notify`, `gitwatch`, `engine-recovery`, `web` | yes |
| [`acceptance`](#acceptance) | `wiring` | yes |

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
- **Notes from `engine-detection`:** an `auto` deployment is left in
  `detected` by detection, ready to apply; there is no separate "approved"
  state for `auto`. `deployments.job_spec` already holds the exact parsed job
  to register (task-group `Count` already adjusted for any `scaling` policy):
  use it as is, do not re-parse or re-plan before the CAS register beyond
  what invariant 1 already requires. `engine.Nomad`/`engine.Store` in
  `internal/engine/engine.go` are the small interfaces detection declared
  over `nomadx`/`store`; extend them (or declare sibling ones next to them)
  rather than duplicating. `Store.LatestDeployment` and `store.Transition`'s
  full rules (allowed table, `ErrStateConflict`) are already in place. Watch
  out for a deployment superseded by detection concurrently with an apply in
  progress: detection never touches `pre_hook`/`applying`/`post_hook`, so
  this cannot happen from that side.

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
- **Notes from `engine-detection`:** a `none` policy job never gets a
  deployment, so its drift for the dashboard comes from
  `Engine.Observations()` (one entry per managed job, redacted diff
  included), not from the store; it is rebuilt every cycle and empty for a
  job no longer in the repo. `deployments.job_spec` is never redacted (see
  [engine-detection](design/engine-detection.md#job_spec-keeps-the-full-unredacted-spec)):
  never render that column.

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
