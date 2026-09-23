# nops

A semi-automatic GitOps controller for [HashiCorp Nomad](https://www.nomadproject.io/).

nops watches a git repo of Nomad job definitions (HCL), compares them with the
live cluster, and brings them in line according to a **policy you set per
job**: apply automatically, wait for a human approval, or only observe. It
keeps its own state in SQLite, shows pending changes in a PR-style dashboard,
and can run **pre/post deployment hooks** (regular Nomad jobs that nops
dispatches for you) around each deployment.

> **Status: early development.** The design is settled and documented; only the
> meta-key parser (`internal/meta`) and the SQLite store (`internal/store`) are
> implemented so far. It is not usable yet.

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

Hooks are ordinary Nomad jobs; [`examples/`](examples/) has ready-made ones:

- [`migrate-hook.nomad.hcl`](examples/migrate-hook.nomad.hcl): run database
  migrations before the new version starts.
- [`smoke-hook.nomad.hcl`](examples/smoke-hook.nomad.hcl): check that the
  service answers after the deploy.
- [`prepull-hostvolume.nomad.hcl`](examples/prepull-hostvolume.nomad.hcl):
  pre-pull a heavy image on the node that holds a host volume, so a
  stop+start deploy only pays for the restart, not for the download.

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

See [docs/development.md](docs/development.md) for the testing strategy and
conventions, and [CLAUDE.md](CLAUDE.md) for the working rules.
