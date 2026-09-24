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

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`web`](#web) | `engine-detection`, `config`, `web-auth` | yes |
| [`wiring`](#wiring) | `config`, `notify`, `gitwatch`, `engine-recovery`, `web` | yes |
| [`acceptance`](#acceptance) | `wiring` | yes |

### web

The dashboard pages and the git webhook, on top of the login
(`web-auth`).

- **Read first:** [dashboard.md](dashboard.md) (including
  [authentication](dashboard.md#authentication)), [policies.md](policies.md#approval),
  [error-handling.md](error-handling.md).
- **Scope:** `internal/web`. Pending deployments with the redacted diff and
  approve/reject, history, the deployment page at `/deployments/<id>`, drift
  of `none` jobs, a health check, and the git webhook endpoint that fires the
  `gitwatch` trigger. Approve and reject call `Engine.Approve`/`Reject`
  (`engine-apply`'s decision 1) with the actor and the `spec_hash` shown on
  the page; the dashboard never calls `Store.Transition` for a decision or
  picks the next state itself.
- **Ready: yes.** The webhook is authenticated with a secret per forge, decided
  in the [decision log](design/decisions.md) (2026-09-24): a new option
  `-webhook-secret-file` (with `NOPS_WEBHOOK_SECRET`, like the other secrets;
  `config` and its docs table change with it), the HMAC-SHA256 of the body for
  GitHub (`X-Hub-Signature-256`) and Gitea (`X-Gitea-Signature`), a
  constant-time comparison of `X-Gitlab-Token` for GitLab. The payload is
  otherwise ignored.
- **Done when:** `httptest` tests for the handlers (every page goes through
  `Auth.Require`), approve/reject with the actor, the webhook with each forge's
  signature and a wrong one. There is no coverage target. Complete
  [dashboard.md](dashboard.md), which is still marked "to be completed".
- **Notes from `web-auth`:** wrap every page and every write in
  `Auth.Require(...)`; the actor comes from `web.UserFrom(r.Context())`.
  `Auth.Register(mux)` adds `/auth/login`, `/auth/callback` and
  `POST /auth/logout`; the git webhook and the health check stay outside
  `Require` (the webhook has its own secret). There is no CSRF token to put in
  a form: `Require` already refuses a cross-origin write. The 403 of the old
  header design is now the allowlist at login, and a request without a session
  is a redirect to the login (`GET`) or 401. Show the actor and a logout button
  (`POST /auth/logout`) in the page header.
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
- **Notes from `web-auth`:** `web.NewAuth(web.AuthOptions{Issuer:
  cfg.OIDCIssuerURL, ClientID: cfg.OIDCClientID, ClientSecret:
  cfg.OIDCClientSecret, RedirectURL: cfg.PublicURL + "/auth/callback",
  AllowedUsers: cfg.OIDCAllowedUsers, AllowedGroups: cfg.OIDCAllowedGroups,
  Log: log})`. It does not contact the provider (an outage must not stop
  nops), so its error is a configuration error and fatal. Never log the
  client secret.
- **Notes from `engine-recovery`:** there is no recovery call to make: start
  `Engine.RunApply` in its own goroutine, and its first cycle (before the
  ticker) is the recovery pass. It does not block, so `web` and detection can
  start right after; a hook step in it can run for a hook's whole timeout.
- **Done when:** `go build ./...` produces the binary and it starts against a
  `nomad agent -dev` (a smoke test in `tests/integration`).

### acceptance

- **Read first:** [development.md](development.md#integration) (manual
  checklists for the real cluster).
- **Scope:** `docs/acceptance/`: one checklist per realistic scenario (heavy
  image pre-pull on a host volume, backup before a stateful deploy, approval
  flow). The README status note changes from "early development" to usable.
- **Done when:** the checklists exist and were run once against the real cluster.
