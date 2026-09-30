<p align="center">
  <img src="docs/assets/logo.webp" alt="Nops logo" width="300">
</p>

# Nops

[![CI](https://github.com/music-gang/nops/actions/workflows/ci.yml/badge.svg)](https://github.com/music-gang/nops/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/music-gang/nops)](LICENSE)

A semi-automatic GitOps controller for [HashiCorp Nomad](https://www.nomadproject.io/).

Nops watches a git repo of Nomad job definitions (HCL), compares them with the
live cluster, and brings them in line according to a **policy you set per
job**: apply automatically, wait for a human approval, or only observe. It
keeps its own state in SQLite, shows pending changes in a PR-style dashboard,
and can run **pre/post deployment hooks** (regular Nomad jobs that Nops
dispatches for you) around each deployment.

> **Status: early development.** The design is settled and documented and
> every building block is implemented and tested end to end against a real
> Nomad; what is left is using it on a real cluster. What each release
> changed is in its [release notes](https://github.com/music-gang/nops/releases), what is left in the
> [issues](https://github.com/music-gang/nops/issues).

Inspired by [gerrowadat/nomad-gitops](https://github.com/gerrowadat/nomad-gitops),
but written from scratch and extended with persistent state, approvals and hooks.
Aimed at a self-hosted cluster for personal use: no enterprise-scale machinery.

**Try it:** [getting started](docs/getting-started.md) takes one machine from
nothing to an approved deployment in about fifteen minutes.

## How it works

1. Nops reads the job files from a git repo and asks Nomad to parse them
   (`/v1/jobs/parse`), so Nomad stays the only HCL interpreter.
2. For each managed job it runs `nomad job plan` and, if there is a
   difference, creates a **deployment** whose state is stored in SQLite.
3. What happens next depends on the job's policy:

   | Policy | Behaviour |
   |---|---|
   | `auto` | Nops applies the change on its own. |
   | `approval` | The deployment waits in the dashboard, with its diff, until someone approves or rejects it. |
   | `none` | The drift is shown but never applied (default). |

4. Applying is a plain `job register` protected by Nomad's check-index (CAS): if
   the job changed since Nops looked at it, the write is rejected instead of
   overwriting someone else's change.

```
detected → [pending_approval] → [pre_hook] → applying → [post_hook] → completed
                                     │            │           │
                                     └────────────┴───────────┴──→ failed
```

## Configuring a job

Policy and hooks live in the job's own HCL, as flat `meta` keys:

```hcl
job "api" {
  meta {
    nops_managed   = "true"
    nops_policy    = "approval"                # auto | approval | none
    nops_pre_hook  = "api-backup,api-migrate"  # parameterized batch jobs, run in this order
    nops_post_hook = "api-smoke"
  }
  # ...
}
```

If a pre-hook fails or times out, the deployment stops (the hooks after it do
not run), the live job is left untouched and you get a notification. Every key
is in the [meta-keys reference](docs/meta-keys.md); how to write a hook, with
runnable [examples](examples/) for a migration, a backup, a smoke test and
pre-pulling an image, is in the [hooks guide](docs/hooks.md).

## Nomad compatibility

Nops is tested against **Nomad 2.0.7**: the integration tests run on that
version in CI. Other 2.0.x releases should work; a new minor is supported once
the same tests pass on it. Nops relies on a few behaviours of Nomad's API that
are not part of a contract (the wording of a check-index failure, the names of
the fields of a plan diff); each has an integration test, so an upgrade that
changes one fails there. How to move to a new version:
[development](docs/development.md#nomad-version).

## Documentation

| I want to… | Read |
|---|---|
| try Nops on one machine | [Getting started](docs/getting-started.md) |
| run it on a cluster, or upgrade it | [Running Nops](docs/running-nops.md) |
| choose how a job is applied | [Policies](docs/policies.md) |
| write a hook | [Hooks](docs/hooks.md) |
| look up an option or a meta key | [Configuration](docs/configuration.md), [meta keys](docs/meta-keys.md) |
| understand how it works, and why | [Philosophy](docs/philosophy.md), [architecture](docs/architecture.md) |
| see what changed in a release | [Releases](https://github.com/music-gang/nops/releases) |
| contribute | [Contributing](CONTRIBUTING.md) |

Everything else is in the [documentation index](docs/README.md).

## License

Licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Music Gang.
