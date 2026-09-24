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
| `engine-apply` | Apply loop: approve/reject, register, health, timeout, the anti-loop rule and blocked drift ([design](design/engine-apply.md)) | `internal/engine` |

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`engine-recovery`](#engine-recovery) | `engine-apply` | plan first |
| [`web`](#web) | `engine-detection`, `config` | plan first |
| [`wiring`](#wiring) | `config`, `notify`, `gitwatch`, `engine-recovery`, `web` | yes |
| [`acceptance`](#acceptance) | `wiring` | yes |

### engine-recovery

Resume what a restart interrupted.

- **Read first:** [state-machine.md](state-machine.md#recovery-after-a-crash),
  [engine-apply.md](design/engine-apply.md) (decision 8: every apply step is
  resume-safe on its own).
- **Scope:** narrowed by `engine-apply`'s design to (a) calling `engine-apply`'s
  same per-deployment step once for every active deployment at startup,
  before its ticker starts, so a crash is noticed immediately rather than
  after up to one `-engine-interval`, and (b) the crash-window table tests
  below. `detected` and `pending_approval` need nothing at startup.
- **Ready: plan first.** What is left open: whether any crash window needs
  more than calling the step function again (the design assumes not).
- **Done when:** a table-driven test per case, including a crash between
  `Dispatch` and the saved child ID and a CAS conflict after restart.
- **Notes from `engine-apply`:** the per-deployment step function is
  `Engine.applyStep` (unexported); exporting it (or a thin wrapper) is this
  task's to decide. `Engine.RunApply`'s in-flight guard (`startApply`/
  `finishApply`) already keeps two overlapping callers from stepping the same
  deployment twice, so calling `applyStep` once per active deployment at
  startup and then starting `RunApply` needs no extra locking.

### web

The dashboard and the git webhook.

- **Read first:** [dashboard.md](dashboard.md), [policies.md](policies.md#approval),
  [error-handling.md](error-handling.md).
- **Scope:** `internal/web`. Pending deployments with the redacted diff and
  approve/reject, history, auth from the proxy header (403 on a write without
  it), POST only with a CSRF token, the git webhook endpoint that fires the
  `gitwatch` trigger. Approve and reject call `Engine.Approve`/`Reject`
  (`engine-apply`'s decision 1) with the actor and the `spec_hash` shown on
  the page; the dashboard never calls `Store.Transition` for a decision or
  picks the next state itself.
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
- **Notes from `engine-apply`:** an `Observation` with `BlockedBy` set means a
  retry rule is suppressing a new deployment for that job's drift (either the
  existing failed/rejected-with-unchanged-index rule, or the anti-loop rule
  for a deployment that failed after applying); render `BlockedReason` next
  to the drift so this is never silently stuck. See
  [engine-apply.md](design/engine-apply.md), decisions 6 and 7.

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
