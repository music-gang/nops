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
- **Git:** no commits, pushes or merges without an explicit request. Small
  commits in Angular style (`type(scope): subject`, see
  `docs/development.md#commit-messages`), with plain messages: what changes
  and why, no superlatives.

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
