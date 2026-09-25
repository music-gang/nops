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
| `orphan-jobs` | A job nops deployed that is gone from the repository but still runs in Nomad is reported (sync state **Not in git**, Needs attention, a notice on its page) and never stopped; the check is suspended while a file does not parse ([design](design/engine-detection.md#orphan-jobs)) | `internal/engine`, `internal/store`, `internal/web` |
| `hook-revisions` | An approval covers the hooks that run: a deployment freezes its hooks (`deployment_hooks`), `spec_hash` covers them, each is registered in Nomad as `<hook-id>-<8 hex>` right before its dispatch and unused revisions are deregistered without purge; the per-cycle hook sync is gone ([hooks](hooks.md#hook-revisions), [design](design/engine-detection.md#hook-revisions)) | `internal/store`, `internal/engine`, `internal/nomadx`, `internal/web` |
| `multi-hooks` | Several pre-hooks and post-hooks per job (`nops_pre_hook = "backup,migrate"`), run in order and stopping at the first failure; the timeout moves to `nops_timeout` on the hook job ([hooks](hooks.md#several-hooks)) | `internal/meta`, `internal/store`, `internal/hooks`, `internal/engine`, `internal/web`, `examples/` |
| `e2e` | Simulated end-to-end integration tests in place of manual acceptance checklists: approval flow, self-heal under `auto`, the pre-hook scenarios (pre-pull, backup) with `raw_exec`, and `examples/` kept parsing ([development](development.md#integration)); the real check is using nops on the cluster ([first rollout](development.md#first-rollout-on-a-real-cluster)) | `tests/integration` |
| `dashboard-live` | Overview, Jobs, Job and Activity poll themselves every 5s (`hx-select`/`hx-swap="outerHTML"` on their own address, the Deployment page's own htmx pattern, no new endpoint), so a new deployment, a changed sync state or *Fetch now* shows up without a reload; the Job page's drift diff stays out of the poll so its `<details>` state is not reset. Also relabels the Overview's git strip ("committed" vs "repository checked") so the two times are not misread as one ([dashboard](dashboard.md#look-and-technology), [decisions](design/decisions.md) 2026-09-25) | `internal/web` |

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`dashboard-polish`](#dashboard-polish) | `dashboard-ux` | yes |

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
