# Nops documentation

Nops is a GitOps controller for HashiCorp Nomad: it deploys the jobs in a git
repository according to a per-job policy. New here? Start with the
[project README](../README.md).

## Tutorial

- [Getting started](getting-started.md): try Nops on one machine.

## How-to guides

- [Running Nops](running-nops.md): run it on a cluster, set up the token, the
  login and the webhook, upgrade.
- [Hooks](hooks.md): run a backup, a migration, or a smoke test around a
  deployment.

## Reference

- [Configuration](configuration.md): every option.
- [Meta keys](meta-keys.md): every `nops_*` key.
- [Dashboard](dashboard.md): pages, login, secret redaction.
- [API](api.md): act on Nops from a script with a token.
- [Command-line client](cli.md): inspect and act on Nops from a terminal.
- [Logs and notifications](logs-and-notifications.md)
- [Metrics](metrics.md)
- [Glossary](glossary.md)

## Explanation

- [Policies](policies.md): auto, approval, none; sync windows and pauses.
- [Philosophy](philosophy.md): the invariants Nops never breaks.
- [Architecture](architecture.md): how Nops works inside.
- [Deployment lifecycle](deployment-lifecycle.md): the states of a deployment.

## Contributing

- [Contributing](../CONTRIBUTING.md) and [development](development.md).
- [Archive](archive/): design decisions up to v0.4.0, no longer updated.
