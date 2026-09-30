# Nops documentation

Nops is a semi-automatic GitOps controller for HashiCorp Nomad: it reads HCL
jobs from a git repo, compares them with the cluster and applies them
according to a per-job policy (`auto` / `approval` / `none`), with state in
SQLite, an approval dashboard and pre/post deployment hooks.

New here? Start with the [project README](../README.md).

## Index

**Understanding Nops**
- [vocabulary.md](vocabulary.md): the terms used in code, docs and PRs.
- [philosophy.md](philosophy.md): principles and invariants, with the reasoning.
- [architecture.md](architecture.md): components, loops, apply and downtime.
- [state-machine.md](state-machine.md): states, transitions, schema, recovery.

**Using it**
- [getting-started.md](getting-started.md): a first deployment on one machine, step by step.
- [running-nops.md](running-nops.md): Nops on a cluster: the Nomad job, the token, a proxy, the login, upgrading.
- [meta-keys.md](meta-keys.md): reference for the `nops_*` meta keys.
- [policies.md](policies.md): `auto`, `approval`, `none`.
- [hooks.md](hooks.md): the hook contract and examples (in [`../examples/`](../examples/)).
- [configuration.md](configuration.md): flags and environment variables.
- [dashboard.md](dashboard.md): the pages, approval, authentication.
- [logs-and-notifications.md](logs-and-notifications.md): what is logged, notifications and their payload.

**Contributing**
- [development.md](development.md): tests, coverage, commands, the changelog,
  releasing, Go conventions.
- [design/decisions.md](design/decisions.md): the decision log up to v0.4.0,
  kept as history.
- [design/gitwatch.md](design/gitwatch.md): how the git repository is read.
- [design/engine-detection.md](design/engine-detection.md): how drift is
  detected and deployments are created, superseded and revalidated.
- [design/engine-apply.md](design/engine-apply.md): how a deployment is moved
  from approval to `completed`.

## Keeping the docs true

Each fact is written in one page and linked from the others. Which page a
change updates is in
[development.md](development.md#if-you-change-x-update-y), and what each
release changed is in the [changelog](../CHANGELOG.md).
