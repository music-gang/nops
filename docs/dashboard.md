# Dashboard

The dashboard shows what Nops is doing and is where you approve, reject,
retry, pause, and resume deployments. Every page needs a
[login](#authentication).

## Pages

- **Overview** (`/`): what needs you (approvals, blocked jobs, recent
  failures, meta errors, paused jobs, orphans), what is deploying, the latest
  completed deployments, and the state of git and detection. *Fetch now* asks
  for a git poll without waiting for the interval.
- **Jobs** (`/jobs`): every managed job with its [sync state](#sync-state-of-a-job),
  policy and last deployment, filterable by state.
- **Job** (`/jobs/{namespace}/{job}`): the job's drift, hooks, sync window,
  [Nomad panel](#the-nomad-panel), meta issues, and deployments. Retry, pause
  and resume a job from here.
- **Deployment** (`/deployments/{id}`): the plan diff, the hook runs, the
  timeline and, while it waits for you, what approving does and the
  *Approve* and *Reject* buttons. *Promote* appears when Nomad waits for you
  to promote canaries.
- **Activity** (`/history`): every deployment, newest first, grouped by day.

Pages refresh themselves. Approving checks the spec
hash you reviewed: if the diff changed since you loaded the page, Nops refuses
and shows the new one.

`/healthz` and [`/metrics`](metrics.md) need no login.

### Sync state of a job

Where a job stands against git. The first state that applies wins.

| State | Meaning |
|---|---|
| **Invalid meta** | A `nops_*` key has an error: Nops ignores the job's policy and hooks. |
| **Paused** | Someone [paused](deployment-lifecycle.md#holding-a-job) the job: Nops starts no deployment for it. |
| **Blocked** | A failed or rejected deployment holds the drift back until a retry or a new commit. |
| **Not in git** | An [orphan](architecture.md#orphan-jobs): deployed by Nops, removed from git, still running. |
| **Awaiting approval** | Its latest deployment waits for a person. |
| **Deploying** | Its latest deployment is in progress. |
| **Held** | It drifts and its [sync window](policies.md#sync-window) is closed. |
| **Drift** | The cluster differs from git and nothing is deploying it. |
| **In sync** | The cluster matches git. |

### The Nomad panel

A read-only box on the Job page, and on a Deployment page while it applies,
with what Nomad reports about the job: status, allocations per task group,
and the latest Nomad deployment with its canaries and health. Use it to see
why a deployment stays in `applying` without opening Nomad's UI.

### Links into the Nomad UI

Set [`-nomad-ui-url`](configuration.md#nomad) and the Job and Deployment
pages link to the job in Nomad's UI, and to its deployments when canaries wait
for promotion. Unset, there are no links.

## Authentication

Nops checks who you are itself; it never trusts a header from a reverse proxy.
`-auth-mode` picks one of two backends (options in
[configuration](configuration.md#dashboard)):

- **`oidc`**: log in through your OpenID Connect provider (Authentik,
  Authelia, Keycloak). Only users or groups on the allowlist get in, and Nops
  refuses to start with an empty allowlist. Nops contacts the provider only at
  login, so a provider outage doesn't stop deployments.
- **`basic`**: [local users](#local-users--auth-modebasic) from a file.

A session lasts 12 hours. Restarting Nops logs everyone out. Changes to the
allowlist or the users file take effect at the next login.

## Local users (`-auth-mode=basic`)

`-users-file` holds one `username:bcrypt-hash` line per user. Generate a line
with:

```sh
htpasswd -nB alice
```

Nops reads the file at startup: restart it after you change a user. There is
no rate limit on failed logins, so put a rate-limiting proxy in front if the
dashboard is reachable from untrusted networks.

## Secret redaction

Nops redacts the plan diff before saving or showing it: a secret value
becomes `<redacted>`, and you still see which field changed. It redacts:

- every environment variable (`Env[...]`);
- template bodies, artifact headers and service check headers;
- any field whose name contains `password`, `token`, `secret`, `auth`,
  `credential`, `privatekey` or `apikey`;
- the `user:password@` part of a URL.

A secret in a field with a harmless name, like
`args = ["--db-password=..."]`, stays visible. Keep secrets in `env` or in a
`template`. Nomad Variables and Vault are better still. The rules live in
[`internal/redact`](../internal/redact/redact.go).
