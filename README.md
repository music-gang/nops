# nops

[![CI](https://github.com/music-gang/nops/actions/workflows/ci.yml/badge.svg)](https://github.com/music-gang/nops/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/music-gang/nops)](LICENSE)

A semi-automatic GitOps controller for [HashiCorp Nomad](https://www.nomadproject.io/).

nops watches a git repo of Nomad job definitions (HCL), compares them with the
live cluster, and brings them in line according to a **policy you set per
job**: apply automatically, wait for a human approval, or only observe. It
keeps its own state in SQLite, shows pending changes in a PR-style dashboard,
and can run **pre/post deployment hooks** (regular Nomad jobs that nops
dispatches for you) around each deployment.

> **Status: early development.** The design is settled and documented and
> every building block is implemented and tested end to end against a real
> Nomad; what is left is using it on a real cluster. What is done and what
> is left is in the [roadmap](docs/roadmap.md).

Inspired by [gerrowadat/nomad-gitops](https://github.com/gerrowadat/nomad-gitops),
but written from scratch and extended with persistent state, approvals and hooks.
Aimed at a self-hosted cluster for personal use: no enterprise-scale machinery.

## How it works

1. nops reads the job files from a git repo and asks Nomad to parse them
   (`/v1/jobs/parse`), so Nomad stays the only HCL interpreter.
2. For each managed job it runs `nomad job plan` and, if there is a
   difference, creates a **deployment** whose state is stored in SQLite.
3. What happens next depends on the job's policy:

   | Policy | Behaviour |
   |---|---|
   | `auto` | nops applies the change on its own. |
   | `approval` | The deployment waits in the dashboard, with its diff, until someone approves or rejects it. |
   | `none` | The drift is shown but never applied (default). |

4. Applying is a plain `job register` protected by Nomad's check-index (CAS): if
   the job changed since nops looked at it, the write is rejected instead of
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
    nops_managed          = "true"
    nops_policy           = "approval"      # auto | approval | none
    nops_pre_hook         = "api-migrate"   # a parameterized batch job
    nops_pre_hook_timeout = "10m"
    nops_post_hook        = "api-smoke"
  }
  # ...
}
```

If a pre-hook fails or times out, the deployment stops, the live job is left
untouched and you get a notification. See the
[meta-keys reference](docs/meta-keys.md) and the [hooks guide](docs/hooks.md).

### Hook examples

Hooks are ordinary Nomad jobs; [`examples/`](examples/) has ready-made,
runnable ones, each paired with the job it deploys:

- [`examples/migrate/`](examples/migrate/): run database migrations before
  the new version starts.
- [`examples/approval/`](examples/approval/): check that the service answers
  after the deploy.
- [`examples/prepull-hostvolume/`](examples/prepull-hostvolume/): pre-pull a
  heavy image on the node that holds a host volume, so a stop+start deploy
  only pays for the restart, not for the download.
- [`examples/backup-stateful/`](examples/backup-stateful/): back up a
  stateful service before its deploy.

The integration tests check that they still parse, that their hooks exist and
that they need nothing but Nomad (no Consul).

## Design principles

- **Plan before every write, CAS on every write.**
- **Never auto-apply a job whose policy is `approval`.**
- **Git is the source of truth** for policy and hooks; nops never writes to git
  or into the live job's meta.
- **One active deployment per job**, enforced by the database.
- **State is saved before acting**, so a crash mid-deployment can be resumed
  without repeating side effects.

The reasoning behind each is in [docs/philosophy.md](docs/philosophy.md).

## Documentation

The full documentation is in [`docs/`](docs/README.md): architecture, the state
machine, policies, hooks, the dashboard, error handling and the decision log.

## Development

Requires Go and, for integration tests, a local `nomad agent -dev`.

```sh
go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
```

See [docs/development.md](docs/development.md) for the testing strategy, the
PR workflow and conventions, [CONTRIBUTING.md](CONTRIBUTING.md) for the short
version, and [CLAUDE.md](CLAUDE.md) for the working rules.

## License

Licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Music Gang.
