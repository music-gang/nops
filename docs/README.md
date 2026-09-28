# nops documentation

nops is a semi-automatic GitOps controller for HashiCorp Nomad: it reads HCL
jobs from a git repo, compares them with the cluster and applies them
according to a per-job policy (`auto` / `approval` / `none`), with state in
SQLite, an approval dashboard and pre/post deployment hooks.

New here? Start with the [project README](../README.md).

## Index

**Understanding nops**
- [vocabulary.md](vocabulary.md): the terms used in code, docs and PRs.
- [roadmap.md](roadmap.md): what is done, in flight and left.
- [philosophy.md](philosophy.md): principles and invariants, with the reasoning.
- [architecture.md](architecture.md): components, loops, apply and downtime.
- [state-machine.md](state-machine.md): states, transitions, schema, recovery.

**Using it**
- [meta-keys.md](meta-keys.md): reference for the `nops_*` meta keys.
- [policies.md](policies.md): `auto`, `approval`, `none`.
- [hooks.md](hooks.md): the hook contract and examples (in [`../examples/`](../examples/)).
- [configuration.md](configuration.md): flags and environment variables.
- [dashboard.md](dashboard.md): the pages, approval, authentication.
- [error-handling.md](error-handling.md): fail loud, logs, notifications.

**Contributing**
- [development.md](development.md): tests, coverage, commands, Go conventions.
- [design/decisions.md](design/decisions.md): the decision log.
- [design/gitwatch.md](design/gitwatch.md): how the git repository is read.
- [design/engine-detection.md](design/engine-detection.md): how drift is
  detected and deployments are created, superseded and revalidated.
- [design/engine-apply.md](design/engine-apply.md): how a deployment is moved
  from approval to `completed`.

## If you change X, update Y

| Change | Update |
|---|---|
| New meta key or different valid values | `internal/meta` **and** `meta-keys.md` (its table is tested against the keys) |
| Behaviour of a policy | `policies.md` |
| Hook contract or dispatch meta | `hooks.md` and the example in `examples/` |
| A scenario in `examples/` | `TestExamplesParse` checks every example; a new scenario needs nothing else unless it shows new behaviour worth an end-to-end test |
| State, transition, schema, recovery | `state-machine.md` |
| Detection cycle (parse, plan, create, supersede, freeze hooks, revision GC) | `internal/engine` **and** `design/engine-detection.md` |
| Apply loop (approve, reject, register, health, timeouts, retry) | `internal/engine` **and** `design/engine-apply.md` |
| Which files are read from git, the git watcher | `internal/gitwatch` **and** `design/gitwatch.md` |
| A dashboard page, route, sync state or login | `internal/web` **and** `dashboard.md` |
| What is logged, or which transition notifies | `error-handling.md` (the one place that says it: other pages link to it) |
| Flag or env var | `internal/config` **and** `configuration.md` (its tables are tested against the options: name, variable, default) |
| Notification adapter or payload | `internal/notify` **and** `error-handling.md#notifications` |
| A test is renamed or deleted | every page that names it (`docs/docs_test.go` fails otherwise) |
| New concept, or a renamed term | `vocabulary.md` |
| A task is finished, added or reshaped | `roadmap.md` |
| Design decision | `design/decisions.md` (and the relevant document) |
| Build or release (`Dockerfile`, `.goreleaser.yaml`, `release.yml`), image tags, semver policy | `development.md#releasing` |
| Invariant | `philosophy.md` **and** the list in CLAUDE.md |
