<p align="center">
  <img src="docs/assets/logo.webp" alt="Nops logo" width="300">
</p>

# Nops

[![CI](https://github.com/music-gang/nops/actions/workflows/ci.yml/badge.svg)](https://github.com/music-gang/nops/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/music-gang/nops)](LICENSE)

A semi-automatic GitOps controller for [HashiCorp Nomad](https://www.nomadproject.io/).

Nops watches a git repository of Nomad jobs and deploys them according to a
**policy you set per job**: automatically, after a person approves, or never.
It shows pending changes in a dashboard and can run **hooks**, Nomad jobs
that run before or after a deployment.

> **Status: early development.** Built for a self-hosted cluster for personal
> use. Inspired by [gerrowadat/nomad-gitops](https://github.com/gerrowadat/nomad-gitops).

**Try it:** [getting started](docs/getting-started.md) takes about fifteen
minutes on one machine.

## How it works

1. Nops reads the job files from git and has Nomad parse them.
2. It plans each managed job and, if there is a difference, creates a
   **deployment**.
3. The job's policy decides what happens next:

   | Policy | Behaviour |
   |---|---|
   | `auto` | Nops deploys the change. |
   | `approval` | The deployment waits in the dashboard until someone approves or rejects it. |
   | `none` | Nops shows the drift and deploys nothing. The default. |

4. Nops registers the job only if nobody changed it since the plan.

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

Every key is in the [meta keys reference](docs/meta-keys.md). To write a hook,
see the [hooks guide](docs/hooks.md) and the [examples](examples/).

## Nomad compatibility

Nops is tested against **Nomad 2.0.7**. Other 2.0.x releases should work.

## Documentation

See the [documentation index](docs/README.md). Changes are in the
[release notes](https://github.com/music-gang/nops/releases). To contribute,
see [CONTRIBUTING](CONTRIBUTING.md).

## License

Licensed under the [Apache License 2.0](LICENSE). Copyright 2026 Music Gang.
