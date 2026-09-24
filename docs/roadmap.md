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
| `e2e` | Simulated end-to-end integration tests in place of manual acceptance checklists: approval flow, self-heal under `auto`, the pre-hook scenarios (pre-pull, backup) with `raw_exec`, and `examples/` kept parsing ([development](development.md#integration)); the real check is using nops on the cluster ([first rollout](development.md#first-rollout-on-a-real-cluster)) | `tests/integration` |

## Todo

| Task | Depends on | Ready |
|---|---|---|
| [`dashboard-ux`](#dashboard-ux) | `dashboard-data` | yes |
| [`dashboard-polish`](#dashboard-polish) | `dashboard-ux` | yes |

What else remains is using nops on a real cluster; what that turns up becomes
new tasks here.

### dashboard-ux

The redesigned dashboard: the right pages with the right data, in a dense
GitHub Primer look. Decided with the maintainer, see the
[decision log](design/decisions.md) (2026-09-24, "redesigned for what an
operator does with it").

- **Read first:** [dashboard.md](dashboard.md), the decision above,
  [engine-apply.md](design/engine-apply.md) (decisions 7 and 9),
  [state-machine.md](state-machine.md).
- **Scope:** `internal/web` (templates, CSS, handlers), and the meta parsing
  of a stored spec for the hooks a deployment *will* run (keys and job IDs
  only; `job_spec` is never rendered).
  - **Overview `/`:** a strip with the git head (short SHA, subject, checked N
    ago, *Fetch now*) and the last cycle (ok or the error, N ago, jobs skipped
    and files unparsed). **Needs attention**, one line each with its reason and
    a link: pending approvals, recent failures, blocked jobs (with *Retry*),
    jobs with invalid meta. **In progress**: active deployments and their phase.
  - **Jobs `/jobs`** (`/drift` redirects): one row per managed job, `none`
    included: policy, sync state (in sync, drift, pending, deploying, blocked,
    invalid), last deployment (state, age), file. A filter by state in the query
    string, no JS.
  - **Job `/jobs/{namespace}/{job}`:** policy, configured hooks, file, sync
    state, *Retry* when blocked, the current drift diff and meta issues, then
    the job's deployments (`Store.ListByJob`).
  - **Deployment `/deployments/{id}`:** a review page. A header with the job
    (linked), state, commit (SHA, subject, author, link), policy, age; a diff
    summary (added, changed, removed per task group); the decision panel on top
    when `pending_approval`, saying what Approve will do (pre-hook, register,
    post-hook); the diff at full width; a sidebar of short values with the full
    ones on hover (the full `spec_hash` stays in the approve form); hooks and
    events as one-line tables. The htmx status polling stays.
  - **Activity `/history`:** every deployment grouped by day, one line each,
    a filter by state.
  - **Look:** hand-written CSS with Primer tokens (light and dark, following
    the system), 14px, 6px radius, system fonts (the vendored Inter and
    JetBrains Mono go), one entity per line with ellipsis and the full value
    in `title=`, `timeView{Rel, Full, ISO}` with an injectable clock. At 768px
    or less secondary columns hide and the sidebar goes under the diff. The CSP
    does not change.
- **Done when:** a page test per route and per state (empty, pending, blocked,
  failed, invalid meta), the helpers, the full `spec_hash` present in the
  approve form, the e2e selectors updated, `dashboard.md` rewritten, and a
  decision row replacing the Airbnb one. Checked by hand at 1280px and 390px,
  light and dark: no row wraps, no horizontal scroll.
- **Notes from `dashboard-data`:** `POST /jobs/{ns}/{job}/retry` and
  `POST /fetch` exist and always redirect to `/`. When a button needs to go
  back to another page (the job page, Jobs), add a server-side list of
  destinations chosen by name (`back=jobs` mapped to a constant path): never
  a path or URL from the form, CodeQL flags it (decision log, 2026-09-24). `Engine.Status()` and `Watcher.Status()` are not
  wired into `web` yet: add them to `web.Options` (small interfaces, like
  `Engine`). An event whose `from` and `to` are the same state is a retry
  request: show its message, not an arrow. `commit_subject`/`commit_author`
  are empty for deployments created before the migration. `gitwatch.CommitURL`
  needs the repository URL from `config`; it returns "" when there is no
  usable link.

### dashboard-polish

Fix what using `dashboard-ux` on the real cluster turns up: layout, wording,
empty states. No new features; anything bigger becomes its own task.

- **Done when:** the maintainer has used it for real and the list they gave is
  closed.
