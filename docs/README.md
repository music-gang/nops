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
- [dashboard.md](dashboard.md): approval, authentication.
- [error-handling.md](error-handling.md): fail loud, logs, notifications.

**Contributing**
- [development.md](development.md): tests, coverage, commands, Go conventions.
- [design/decisions.md](design/decisions.md): the decision log.

Not present yet, created together with the code: `acceptance/` (manual
checklists for the real cluster).

## If you change X, update Y

| Change | Update |
|---|---|
| New meta key or different valid values | `internal/meta` **and** `meta-keys.md` |
| Behaviour of a policy | `policies.md` |
| Hook contract or dispatch meta | `hooks.md` and the example in `examples/` |
| State, transition, schema, recovery | `state-machine.md` |
| Flag or env var | `internal/config` **and** `configuration.md` |
| New concept, or a renamed term | `vocabulary.md` |
| A task is finished, added or reshaped | `roadmap.md` |
| Design decision | `design/decisions.md` (and the relevant document) |
| Invariant | `philosophy.md` **and** the list in CLAUDE.md |
