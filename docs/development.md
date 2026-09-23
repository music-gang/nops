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
| `internal/web` | `httptest` for handlers, header auth, CSRF and 403. No coverage target. |
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
- Realistic scenarios (pre-pulling heavy images, backups) are manual
  checklists in `docs/acceptance/`, to be run on the real cluster.

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
   (`feat/config`): that is how the roadmap knows the task is in flight.
2. **One commit**, already in its final [Angular style](#commit-messages):
   it is what GitHub proposes as the squash commit (see below), so amend it
   (`git commit --amend`) rather than adding more while the branch is open.
3. Open a PR **titled in Angular style**, matching the commit subject.
4. When CI is green, **squash and merge**: the prefilled message is the
   branch's own commit, and needs no editing. The branch is deleted
   automatically.

Commits inside a branch do not need to be signed, whoever makes them: the
squash commit on `main` is created and signed by GitHub, and the ruleset does
not require signed commits. Locally the maintainer commits with his GPG key;
in a cloud session Claude commits unsigned and opens the PR
(see [CLAUDE.md](../CLAUDE.md#picking-up-work)).

Merges are squash-only, so `main` is a straight line with one commit per PR,
and **the commit that lands on `main` is the branch's own commit, not the PR
description**: the repository's squash setting is "Default to pull request
title and commit details" (`squash_merge_commit_message = COMMIT_MESSAGES`),
so GitHub prefills the squash box from the commit(s) on the branch. A branch
has exactly **one commit**, already in its final [Angular
style](#commit-messages) (plain prose body, wrapped at 72 characters, no
markdown headings): that is what GitHub proposes and what gets merged,
without anyone editing the box by hand.

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
  branch has **one commit**, already in this final shape — see
  [Workflow and CI](#workflow-and-ci) for how it relates to the PR title and
  to the PR description, which is a separate, richer text.
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
