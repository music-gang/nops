# Development and testing

## What needs which tests

| Area | Kind of test |
|---|---|
| `internal/meta` | Table-driven unit tests: valid values, invalid values, unknown keys, defaults. |
| `internal/engine` | Table-driven unit tests for **every** transition, including crash recovery and CAS conflicts, with an in-memory fake Nomad and an injected clock. |
| `internal/nomadx` | Unit tests against an `httptest` stub of the Nomad API (request shape, error mapping) + integration tests against a real Nomad. |
| `internal/store` | Against **real SQLite** on a temporary file (`t.TempDir()`), not mocks: constraints, the lock index, migrations. |
| `internal/hooks` | Unit tests with an in-memory fake Nomad, a **real SQLite** store and an injected clock (success, failure, timeout, resume, redispatch with token, Nomad/SQLite errors) + integration with `raw_exec` hooks. |
| `internal/redact` | Unit: no secret must reach the DB or the HTML, over hand-built diffs and a fixture from a real plan (`testdata/plan_diff.json`) + integration: the same rules on a live plan. |
| `internal/notify` | `httptest` receivers: the request of every adapter, several adapters at once, and non-2xx, timeout, unreachable and cancelled (a WARN each, no error, no URL in the log). |
| `internal/gitwatch` | Unit tests against a local bare repository over `file://` (new commit, no change, force-push, coalesced triggers, vars pairing, `-git-path` scoping, a failed fetch keeps the snapshot). Skipped if `git` is not on `PATH`. |
| `internal/web` | `httptest` for handlers; the login against a fake OIDC provider (`httptest` serving discovery, keys, token and userinfo, signing real ID tokens), cross-origin refusals and 401/403. No coverage target. |
| `cmd/nops` | Almost entirely straight-line wiring, so almost entirely covered by the integration smoke test below, not unit tests: a unit test only for the one piece of actual logic (`newAuthenticator` picking the login backend by `-auth-mode`). No coverage target. |
| Real interaction with Nomad | Integration. |

## Integration

- Build tag `integration`, in `tests/integration/`. They run against a local
  `nomad agent -dev`. If `NOPS_TEST_NOMAD_ADDR` is not set they are skipped
  (skip, not fail).
- Hooks are tested with **lightweight jobs**, never with heavy images:
  - `raw_exec` (enabled in dev mode): `true` → success, `false` → failure,
    `sleep 600` → timeout;
  - docker driver, when available: `busybox`/`alpine`, to check the passing of
    `nops_image_<task>` and placement via host volume.
- Realistic scenarios (pre-pulling a heavy image, a backup before a stateful
  deploy) are **simulated**, not run on a real cluster: the same guarantees
  (the pre-hook runs before the register and gets the deployment ID, a failing
  or slow hook leaves the live job alone, a failure is not retried in a loop)
  are checked with `raw_exec` hooks that append to a log or write a marker
  file. What a simulation cannot say (how long a real pull takes, whether a
  real dump restores, the cluster's own ACLs and TLS) is judged by using nops
  on the cluster.
- The **end-to-end tests** (`e2e_*_test.go`) build the real `nops` binary once
  and run it as a subprocess against a scratch git repository (a bare repo
  over `file://`, pushed to by the test) and the Nomad under test: basic
  auth, 200ms intervals, the dashboard driven over HTTP and nops's own SQLite
  read alongside it. `TestE2EApprovalFlow` covers create, approve, reject,
  superseding, a stale `spec_hash` (409) and an outside edit under policy
  `approval`; `TestE2EAutoRevertsOutsideEdit` the same edit under `auto`; the
  `TestE2EPreHook*` tests the hook scenarios above. The hooks write to a
  test directory, so they need the Nomad agent on the same host as the tests
  (true for `nomad agent -dev`).
- `TestNopsBinaryStartsAndShutsDown` is the smoke test of the same harness:
  `/healthz` answers, then `SIGTERM` and a clean exit within the shutdown
  grace period.
- `TestExamplesParse` parses every job and hook in `examples/` with Nomad's
  own parser, checks its `nops_*` meta, that each declared hook exists and
  that nothing needs Consul (every service has `provider = "nomad"`, no file
  mentions Consul). It does not run the examples: their docker images are
  not pulled in CI.
- `TestMain` drops the `NOMAD_*` variables of the environment, so a shell set
  up for a real cluster is never used by a test.
- A dev agent answers `429` past 100 connections from one address, so a test
  that creates a Nomad client closes its idle connections when it ends
  (`newClient`).

### First rollout on a real cluster

The real check is using it. The policies allow going in steps: start with
`nops_policy = "none"` on a few jobs and watch `/drift`; move stateful jobs to
`approval` and stateless ones to `auto` one at a time.

## Coverage

≥ 80% on `engine`, `store`, `meta`, `hooks` (`go test -cover`). No target on
`web` and `cmd`.

## When a piece of work is "done"

A commit/PR is complete only if:

1. `go build ./...`, `go vet ./...`, `staticcheck` and `go test ./...` are green;
2. if it touches `engine`, `hooks` or `nomadx`, the integration tests are also
   green against a `nomad agent -dev`;
3. every behaviour change has its test;
4. the docs (and `examples/`) are updated if the HCL syntax, states or a
   decision change.

## Commands

```sh
go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest -tags integration ./...

nomad agent -dev &                     # in another terminal
NOPS_TEST_NOMAD_ADDR=http://127.0.0.1:4646 go test -tags integration -race -count=1 ./tests/integration/...
```

`staticcheck`: with a recent Go, the binary in `~/go/bin` may have been built
with an older Go and fail with "export data version"; `go run ...@latest`
avoids the problem.

## Workflow and CI

`main` is protected by a repository ruleset: a pull request is required, the
checks below must pass on an up-to-date branch, history must be linear, and
force pushes and deletion are blocked. No approvals are required (there is a
single maintainer): CI is the gate.

Flow:

1. Branch from `main`: `type/short-description` (e.g. `feat/hooks-package`).
   For a task in the [roadmap](roadmap.md) the description is the task ID
   (`feat/config`): that is how the roadmap knows the task is in flight. A
   `plan first` task is discussed with the maintainer before this step, in
   the session — there is no branch or PR for the plan itself.
2. Commit as the work happens, each in [Angular style](#commit-messages). A
   later change — a fix, an answer to a review comment — is a **new
   commit**, not an amend or a force-push: the branch's history is free to
   show the work as it went.
3. Open a PR **titled in Angular style** once there is something to review.
4. When CI is green, the maintainer **squashes and merges** by hand, writing
   the final commit from the branch's commits and the PR description. The
   branch is deleted automatically.

Commits inside a branch do not need to be signed, whoever makes them: the
squash commit on `main` is created and signed by GitHub, and the ruleset does
not require signed commits. Locally the maintainer commits with his GPG key;
in a cloud session Claude commits unsigned and opens the PR
(see [CLAUDE.md](../CLAUDE.md#picking-up-work)).

Merges are squash-only, so `main` is a straight line with one commit per PR.
The repository's squash setting is "Default to pull request title and commit
details" (`squash_merge_commit_message = COMMIT_MESSAGES`), which prefills
the squash box from the PR title and the branch's own commit messages — but
the maintainer edits that box by hand before merging, since he is the one
who merges. A branch is free to carry several commits as the work (or a
review round) goes: each one plain and honest in its own
[Angular style](#commit-messages) (wrapped at 72 characters, no markdown
headings), rather than a single commit rewritten every time to look like
`main`'s final shape.

The **PR description** is a separate field, for whoever reviews it on GitHub,
and is free to use real structure:

```markdown
## What changes

- concrete, present-tense bullet points

## Why

- the motivation, one bullet per reason
```

An optional `## Notes` section holds a question that is the maintainer's to
decide (see [CLAUDE.md](../CLAUDE.md#picking-up-work)), or something the next
task needs to know. There is still no PR template file: a template's HTML
comments and an unfilled checklist item are exactly the kind of boilerplate
that once leaked into a commit body (`a933a4c`, `8052989`, back when the
squash setting copied the PR description verbatim), so every section here is
written by hand and filled in, never left as a placeholder. The "done"
checklist stays in
[When a piece of work is "done"](#when-a-piece-of-work-is-done), not in the
PR body. If the branch falls behind `main`, use "Update branch" (or
`git rebase main`).

| Check | Required | What it runs |
|---|---|---|
| `test` | yes | `gofmt` (no unformatted files), `go mod tidy` (no diff), build, `go vet` (also with `-tags integration`), `go test -race -cover ./...` |
| `lint` | yes | `staticcheck` (pinned version, also with `-tags integration`) |
| `integration` | yes | Downloads Nomad (pinned version, SHA256-verified), starts `nomad agent -dev`, runs `go test -tags integration -race -count=1 -v ./tests/integration/...` |
| `pr-title` | yes | The PR title matches `type(scope): subject` with the types listed below and a lowercase subject (at most 72 characters) without trailing period |
| `govulncheck` | no | Known vulnerabilities in dependencies, on PRs, on `main` and weekly. Not required so a new advisory cannot block unrelated PRs |

CodeQL (default setup), secret scanning with push protection, and Dependabot
(Go modules and GitHub Actions, weekly, titled `build(deps): ...` and
`ci(deps): ...`) are enabled on the repository.

Supply-chain rules: every GitHub Action is pinned to a full commit SHA (the
repository enforces it), and the workflow token is read-only. Dependabot
proposes the SHA bumps. The versions of `staticcheck` and `govulncheck` are
pinned in the workflows and bumped by hand.

To run locally what CI runs, use the commands in [Commands](#commands), plus:

```sh
test -z "$(gofmt -l .)" && go mod tidy && git diff --exit-code go.mod go.sum
go vet -tags integration ./...
```

## Commit messages

[Angular style](https://github.com/angular/angular/blob/main/contributing-docs/commit-message-guidelines.md)
(Conventional Commits):

```
<type>(<scope>): <subject>

<body: what changed and why>

<footer: BREAKING CHANGE, issue references, Co-Authored-By>
```

- **Types:** `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
  `build`, `ci`, `chore`, `revert`.
- **Scope** (optional): the package or area, e.g. `meta`, `store`, `engine`,
  `nomadx`, `hooks`, `web`, `gitwatch`, `config`, `notify`, `docs`, `examples`.
- **Subject:** imperative, lowercase, no trailing period, at most 72
  characters (`feat(store): add hook_runs table`).
- **Body:** wrap at 72 characters; plain prose, no markdown headings (this is
  what `git log` shows); explain what and why, not how; no superlatives. A
  branch can carry several commits — see [Workflow and CI](#workflow-and-ci)
  for how the maintainer turns them into the one commit that lands on
  `main`, and how the PR description, a separate and richer text, relates to
  it.
- **Breaking changes** (state machine, schema, HCL meta syntax): add a
  `BREAKING CHANGE:` footer, or `!` after the scope.
- **Attribution:** a commit made by an assistant carries
  `Co-Authored-By: <name> <email>` in the footer, and nothing else — that is
  enough to tell who or what wrote it. **No session link or URL, and no
  "Generated with ..." line**, in the commit or in the PR description: none
  of it holds information for anyone reading `main` later. This holds
  regardless of what a session's own attribution instructions ask for by
  default — this file is the one that applies here.
- One logical change per PR. Code and the docs describing it go in the
  **same** PR (see the rules in CLAUDE.md).

## Go conventions

- Go ≥ 1.22. Format with `gofmt`/`goimports`. Lint: `go vet` + `staticcheck`.
- Package names are short, singular, without underscores. No `util`/`common`
  packages. Identifiers use the terms in [vocabulary](vocabulary.md).
- `context.Context` is always the first argument of any function that does I/O.
- Errors: wrap with `fmt.Errorf("dispatch hook %s: %w", id, err)`. Sentinels or
  types only when the caller needs to tell them apart (e.g. `ErrCASConflict`,
  `ErrHookTimeout`).
- Interfaces are defined by the **consumer** and kept small: `engine` declares
  what it needs from Nomad and from the store.
- No `init()` with side effects and no global state. Time is injected
  (`func() time.Time`) so timeouts are testable.
- Logging: structured `log/slog` (keys in [error-handling](error-handling.md)).
- Every flag has its `NOPS_<NAME>` env var. All intervals and timeouts are
  configurable, nothing hardcoded.
- Dependencies: before writing a library, look for an established package
  (`hashicorp/nomad/api`, `go-git`, `modernc.org/sqlite`, `oklog/ulid`).
