# Nops documentation

Nops is a semi-automatic GitOps controller for HashiCorp Nomad: it reads HCL
jobs from a git repo, compares them with the cluster and applies them
according to a per-job policy (`auto` / `approval` / `none`), with state in
SQLite, an approval dashboard and pre/post deployment hooks. New here? Start
with the [project README](../README.md).

## Using Nops

| I want to… | Read |
|---|---|
| try it on one machine | [Getting started](getting-started.md) |
| run it on a cluster: the Nomad job, the token, a proxy, the login, the webhook, upgrading | [Running Nops](running-nops.md) |
| choose whether a job deploys on its own, waits for an approval or is only watched; pause it; limit when it deploys | [Policies](policies.md) |
| run something before or after a deployment (a backup, a migration, a smoke test) | [Hooks](hooks.md) |
| know what a page of the dashboard shows, or how login works | [Dashboard](dashboard.md) |
| know what Nops logs, and get notified | [Logs and notifications](logs-and-notifications.md) |
| watch Nops itself with Prometheus, and get an alert when it is stuck | [Metrics](metrics.md) |
| look up an option | [Configuration](configuration.md) |
| look up a `nops_*` meta key | [Meta keys](meta-keys.md) |
| know what a word means | [Glossary](glossary.md) |

## Understanding Nops

| I want to know… | Read |
|---|---|
| why it works the way it does, and what it never does | [Philosophy](philosophy.md) |
| how it works inside: reading the repository, detection, apply, hook revisions | [Architecture](architecture.md) |
| every state of a deployment and what moves it, what holds a job back, the database, a restart | [Deployment lifecycle](deployment-lifecycle.md) |

## Working on Nops

| I want to… | Read |
|---|---|
| contribute a change | [Contributing](../CONTRIBUTING.md), then [development](development.md): the flow of a change, tests, releasing, Go conventions |
| know which page a change updates | [If you change X, update Y](development.md#if-you-change-x-update-y) |
| read how an earlier decision was taken | [Archive](archive/): the decision log and the design plans up to v0.4.0, kept as history and not updated |

Each fact is written in one page and linked from the others; tests check the
links, the option and meta-key tables, and the tests the pages name
([development](development.md#what-needs-which-tests)). What each release
changed is in its [release notes](https://github.com/music-gang/nops/releases).
