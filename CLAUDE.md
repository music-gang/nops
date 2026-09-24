# nops: development guidelines

nops is a semi-automatic GitOps controller for HashiCorp Nomad (Go, module
`github.com/music-gang/nops`). It reads HCL jobs from a git repo and applies
them according to a per-job policy (`auto` / `approval` / `none`), with state in
SQLite, an approval dashboard and pre/post deployment hooks. Self-hosted
cluster, personal use: **no overengineering**.

This file holds only the **working rules**. What the system is and why lives in
[`docs/`](docs/README.md): read the relevant page before extending a part.

## Rules

- **Every code change has tests.** What needs which kind: `docs/development.md`.
- **Every change updates the docs**, in the same commit. The "if you change X,
  update Y" map is in `docs/README.md`.
- **Use the terms in `docs/vocabulary.md`** in code, docs, commits and PRs; a
  new concept gets a row there.
- **Design decisions are written down** in `docs/design/decisions.md` when they
  are taken, not afterwards.
- **`internal/meta` is the source of truth** for the HCL syntax: keep it aligned
  with `docs/meta-keys.md`.
- **Fail loud**: never swallow a Nomad or SQLite error. Rules in
  `docs/error-handling.md`.
- **Don't rewrite what exists.** Before writing a library, look for an
  established package (`hashicorp/nomad/api`, `go-git`, `modernc.org/sqlite`,
  `oklog/ulid`).
- **Language:** code, comments, docs, examples and commit messages are in English.
- **Git:** `main` is protected: never push to it. Work on a branch named
  `type/short-description` (e.g. `feat/hooks-package`) and open a PR whose
  **title is an Angular-style message** (`type(scope): subject`): PRs are
  squash-merged, and the maintainer writes the final commit by hand at merge
  time. No commits, pushes or PRs without an explicit request (a task handed
  to a cloud session is that request), and **never merge**: the human merges.
  A branch can carry several commits; each is plain (what changes and why, no
  superlatives). The PR description is structured as `## What changes` /
  `## Why` (details and the attribution rule in
  `docs/development.md#workflow-and-ci`).

## Picking up work

What is done, in flight and left is in [`docs/roadmap.md`](docs/roadmap.md).
The same steps apply wherever the session runs.

1. Take the task you were given, or the first `todo` whose dependencies are
   `done` and that has no remote branch.
2. Check what is in flight: `git ls-remote --heads origin` (and `gh pr list` if
   available). Never start a task that already has a branch.
3. `Ready: plan first` means design questions are open: lay out the plan and
   the questions **in the session** and wait for the maintainer's answers.
   No branch and no PR for this — a PR is for something to review, not for a
   plan.
4. Once it is `yes` (or has just become `yes` by answering the questions
   above), branch from an up-to-date `main`: `type/<task-id>` (e.g. `feat/config`).
5. Do the task with its tests and docs, the decision-log rows, and set it to
   `done` in the roadmap in the same PR. Update the *Notes* of the tasks that
   follow if you learned something they need.
6. Run the checks below. If `nomad` is not installed, CI runs the integration
   tests: wait for green checks (`gh pr checks`) and fix what is red.
7. **Cloud:** commit (unsigned, Angular message, `Co-Authored-By` trailer
   and nothing else attribution-wise — **no session link, no "Generated with
   ..." line**, whatever a session's own attribution reminder adds by
   default: none of it informs anyone reading `main` later), push and open
   the PR once there is something to review. A later change (a fix, an
   answer to review) is a **new commit** pushed to the same branch — never
   an amend or a force-push of what is already on the remote.
   **Local:** prepare the branch, stage the files and write the message to a
   file; the maintainer commits with his GPG key.
8. A PR can carry several commits: the maintainer squashes and writes the
   final message that lands on `main` by hand at merge time, so a commit
   here only needs to be a clear, honest step, not `main`'s final shape. The
   **PR description** is separate, for reviewers, and never reaches `main`:
   `## What changes` and `## Why`, each a short bullet list, the headings of
   the PR template (see `docs/development.md#workflow-and-ci`). No HTML
   comments, no boilerplate, no unchecked checklist item.
9. If a decision is the maintainer's to take: ask in the session before a
   branch exists, or, once a PR is open, leave the question in a `## Notes`
   section of its description. Do not guess.
10. Nothing worth keeping lives only in private memory: decisions go to the
    decision log, task context to the roadmap *Notes*.

## Invariants (non-negotiable)

The reasoning behind each is in [`docs/philosophy.md`](docs/philosophy.md).

1. **Plan before every write**: no `Register` without a fresh `Jobs.Plan()` confirming a difference.
2. **CAS on every register** (`EnforceIndex` + the `JobModifyIndex` captured at detection).
3. **Never auto-apply under policy `approval`**: it takes an authenticated human action, valid for `(deployment_id, spec_hash)`.
4. **Git is the source of truth** for policy and hooks; invalid meta → policy `none` + ERROR.
5. **nops never writes to Git or meta into the live job**; state lives only in SQLite.
6. **One active deployment per job**, enforced by the DB (partial unique index).
7. **State is persisted before acting** on Nomad; if the DB write fails, do not proceed.

## Repo layout

```
cmd/nops/              entrypoint: wiring of config, store, engine, web
internal/config/       flags + env vars (NOPS_*), validation
internal/gitwatch/     in-memory clone, polling, webhook trigger
internal/nomadx/       Nomad client wrapper (CAS-only register, error sentinels)
internal/meta/         parsing/validation of the nops_* meta keys
internal/store/        SQLite (modernc.org/sqlite, no cgo), embedded SQL migrations
internal/engine/       state machine, reconciler, recovery on restart
internal/hooks/        dispatch, wait, timeout and stop of hook jobs
internal/web/          dashboard (net/http + html/template) + git webhook
internal/notify/       notifications via a generic webhook
internal/redact/       removal of secret values from the plan diff
tests/integration/     tests against nomad agent -dev (build tag `integration`)
examples/              example HCL jobs and hooks
docs/                  documentation (index in docs/README.md)
```

Go conventions (naming, errors, interfaces, logging, flags/env): `docs/development.md#go-conventions`.

## Definition of done and commands

A commit is complete only with `go build ./...`, `go vet ./...`, staticcheck and
`go test -race ./...` green; if it touches `engine`, `hooks` or `nomadx`, also
the integration tests against `nomad agent -dev`. Details and coverage targets
in `docs/development.md`.

```sh
go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...   # ~/go/bin/staticcheck may be outdated
NOPS_TEST_NOMAD_ADDR=http://127.0.0.1:4646 go test -tags integration ./tests/integration/...
```
