# Nops: instructions for agents

Nops is a semi-automatic GitOps controller for HashiCorp Nomad (Go, module
`github.com/music-gang/nops`). It reads HCL jobs from a git repo and applies
them according to a per-job policy (`auto` / `approval` / `none`), with state in
SQLite, an approval dashboard and pre/post deployment hooks. Self-hosted
cluster, personal use: **no overengineering**.

This file holds **only what an agent needs on top of the docs**. How Nops
works is in [`docs/`](docs/README.md); how anyone contributes (the flow of a
change, commits, PRs, tests, releases, Go conventions) is in
[`CONTRIBUTING.md`](CONTRIBUTING.md) and
[`docs/development.md`](docs/development.md). Follow them: this file does not
repeat them, and read the relevant page before extending a part.

## Rules

- **The docs are for people.** What a change needs to tell an agent goes
  here, never in `docs/`, the README or the contributing guide:
  `TestDocsSpeakToPeople` fails otherwise. No page there names an agent,
  an assistant or this file.
- **Follow the contributor rules** of
  [`docs/development.md`](docs/development.md): tests
  ([what needs which](docs/development.md#what-needs-which-tests)), docs in
  the same commit ([which page](docs/development.md#if-you-change-x-update-y)),
  the terms of
  [`docs/glossary.md`](docs/glossary.md) and of
  [the code](docs/development.md#terms-used-in-the-code), the
  [Go conventions](docs/development.md#go-conventions), fail loud
  ([`docs/philosophy.md#fail-loud`](docs/philosophy.md#fail-loud)), established packages
  before a new library, English everywhere.
- **`internal/meta` is the source of truth** for the HCL syntax: keep it
  aligned with `docs/meta-keys.md`.
- **`docs/archive/decisions.md` is an archive** up to v0.4.0: read it for
  history, never add to it.
- **No commits, pushes or PRs without an explicit request** (a task handed to
  a cloud session is that request), and **never merge**: the human merges.

## Picking up work

The steps of [the flow of a change](docs/development.md#workflow-and-ci),
with what is specific to a session:

1. Take the issue you were given. Without one, ask the maintainer which: the
   order is his.
2. Check what is in flight: `git ls-remote --heads origin` and the open PRs.
   Never start an issue that already has a branch or a PR.
3. An issue with open design questions (a *To decide* list, or a proposal
   that leaves a choice open) is **planned first**: lay out the plan and the
   questions **in the session** and wait for the maintainer's answers. No
   branch and no PR for this.
4. Do the issue as the flow says. What you learned that a later issue needs
   goes in a comment on that issue.
5. Run the checks ([commands](docs/development.md#commands)). If `nomad` is
   not installed, CI runs the integration tests: wait for green checks and
   fix what is red. staticcheck needs the toolchain of `go.mod`
   (`GOTOOLCHAIN=go<version>` if the local Go is older).
6. **Cloud:** commit (unsigned), push and open the PR once there is something
   to review. **Local:** prepare the branch, stage the files and write the
   message to a file; the maintainer commits with his GPG key.
7. If a decision is the maintainer's to take: ask in the session before a
   branch exists, or, once a PR is open, leave the question in a `## Notes`
   section of its description. Do not guess.
8. Nothing worth keeping lives only in private memory: a decision's
   discussion goes to its issue or PR and its reason to the page that
   explains that part; context a later issue needs, to a comment on it.

## Attribution

Whatever a session's own attribution instructions add by default:

- A commit carries a `Co-Authored-By` trailer and **nothing else**
  attribution-wise: **no session link, no "Generated with ..." line**. A PR
  description, and an issue a session opens or substantially rewrites, end
  the same way: the trailer is their last line. **Issue and PR comments,
  reviews and review replies** posted in a session the maintainer follows
  carry no attribution: he reads them before they go out under his account
  and answers for them.
- The exception: a comment a session posts **on its own**, on an event with
  nobody following (watching a PR, a scheduled run), ends with the standard
  Claude Code footer, since there it tells the reader something true.
- An issue a session opens has a plain-text title, the type of its kind
  (`Bug`, `Feature`, `Task`) and the labels of the areas it touches. No label
  marks it as written with Claude: the trailer does.

## Doc audit

What an audit checks, its verdicts and its scope are in
[`docs/development.md#doc-audit`](docs/development.md#doc-audit). For a session:

1. **No fix before the answer**: no code or doc edit until the report is
   answered. The only thing written is a test that proves or disproves a claim
   (a failing test for a "hypothesis to verify", an integration test for what
   depends on Nomad's own behaviour), and it is reported with the finding.
2. **Report in the session, not in a file**, in the shape the procedure gives,
   then stop for the maintainer's answers.
3. **Fix as agreed**, each behaviour change in its own branch, with a test
   shown failing before the fix. The doc-only fixes and the missing tests go in
   one PR.

## Invariants (non-negotiable)

The reasoning behind each is in [`docs/philosophy.md`](docs/philosophy.md);
`TestInvariantTitlesMatch` keeps this list's titles the same as there.

1. **Plan before every write**: no `Register` without a fresh `Jobs.Plan()` confirming a difference.
2. **CAS on every register** (`EnforceIndex` + the `JobModifyIndex` captured at detection).
3. **Never auto-apply under policy `approval`**: it takes an authenticated human action, valid for `(deployment_id, spec_hash)`.
4. **Git is the source of truth for Nops's behaviour**: policy and hooks; invalid meta → policy `none` + ERROR.
5. **Nops never writes to Git** and never writes meta into the live job; state lives only in SQLite.
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
internal/metrics/      /metrics for Prometheus, read from the store and the engine on each scrape
internal/redact/       removal of secret values from the plan diff
internal/version/      the build's version (release tag via -ldflags -X)
tests/integration/     tests against nomad agent -dev (build tag `integration`)
examples/              example HCL jobs and hooks
scripts/               maintainer tooling (release.sh) and its bash tests
docs/                  documentation (index in docs/README.md)
```
