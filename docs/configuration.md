# Configuration

Every option of the Nops server, `nops serve`. To run Nops on a cluster, see
[running Nops](running-nops.md). The options of the client are in
[command-line client](cli.md).

## Rules

- Every option is in the [configuration file](#configuration-file). Only
  `-config`, `-listen-addr`, `-public-url`, `-db-path`, `-nomad-addr` and
  `-log-level` are also flags. Each still reads an environment variable,
  `NOPS_` plus the flag name in upper case, dashes as underscores
  (`-db-path` → `NOPS_DB_PATH`), but logs a WARN at start naming the
  attribute that replaces it.
- Precedence: flag, then variable, then the
  [configuration file](#configuration-file), then default. An empty variable
  counts as unset.
- Nops ignores the standard `NOMAD_*` variables, so the ones Nomad sets for a
  job don't change what Nops manages.
- Durations use Go syntax (`90s`, `5m`, `1h`) and must be positive.
- Nops reports every invalid option at once and refuses to start.
  `nops config validate <file>` checks a file before it reaches the server.
- `nops serve -h` lists every flag. `nops -version` prints the version.

### Secrets

Every secret is read from the file a `*_file` attribute of the
[configuration file](#configuration-file) names, so it never shows up in
`ps` or in the environment.

## Nomad

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-nomad-addr` | `NOPS_NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad HTTP API address. |

The `nomad` block of the [configuration file](#configuration-file) says how
Nops reaches Nomad. Only `addr` has a flag, which wins. One block at most:

```hcl
nomad {
  addr       = "https://nomad.service.consul:4646"
  namespaces = ["default", "apps"]
  token_file = "/secrets/nomad-token"
  ui_url     = "https://nomad.example.com"
  ca_cert    = "/secrets/nomad-ca.pem"
}
```

| Attribute | Default | Meaning |
|---|---|---|
| `addr` | `http://127.0.0.1:4646` | Nomad HTTP API address. |
| `namespaces` | `["default"]` | The [namespaces](running-nops.md#namespaces) Nops manages. |
| `token_file` | none | File holding the [Nomad ACL token](running-nops.md#the-nomad-token). Unset: no token. |
| `ui_url` | none | Nomad UI address as a browser reaches it, for the dashboard's links. Unset: no links. |
| `ca_cert` | none | PEM file of the CA of the Nomad server certificate. |
| `client_cert` | none | PEM client certificate for mTLS, with `client_key`. |
| `client_key` | none | PEM client key for mTLS, with `client_cert`. |
| `tls_skip_verify` | `false` | Skip server certificate checks. Testing only. |
| `timeout` | `30s` | How long a request to Nomad may take, the answer included. |

## Configuration file

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-config` | `NOPS_CONFIG` | none | HCL file of the options. |

### Repositories

Each `repository` block is a repository of jobs. Nops needs at least one:

```hcl
repository "apps" {
  url        = "https://git.example.com/ops/apps.git"
  branch     = "main"
  path       = "nomad"
  token_file = "/secrets/apps-token"
  jobs       = ["apps/*"]
}
```

| Attribute | Default | Meaning |
|---|---|---|
| `url` | **required** | URL of the repository. |
| `branch` | `main` | Branch to read. |
| `path` | none | Subdirectory holding the job files. Unset: the root. |
| `username` | `git` | Username sent with the token: `oauth2` for GitLab, the user name for Gitea. |
| `token_file` | none | File holding the HTTPS token. Unset: public repository. |
| `jobs` | none | The jobs the repository may declare, as `<namespace>/<job ID>` patterns. Unset: any job. |
| `timeout` | `2m` | How long a poll of the repository may take, the clone included. |

- The name, made of letters, digits, `-` and `_`, labels the repository on
  the dashboard, in metrics, and in notifications.
- A `jobs` pattern follows Go's [`path.Match`](https://pkg.go.dev/path#Match):
  `*` never crosses the `/`. A pattern without a `/` is a job ID in the
  `default` namespace, and `api-*` doesn't match `api`. Nops
  [refuses](architecture.md#rules-detection-keeps) a file whose job no
  pattern matches.
- A name used twice, an empty `jobs`, or a pattern that is malformed or
  matches no managed namespace stops Nops.

Nops reads private repositories over HTTPS with a token. It doesn't support
SSH. The release image can't read `file://` URLs.

### Listen

The `listen` block sets the address of the dashboard's server, its timeouts,
and its [TLS](#certificate-and-key). Only `addr` has a flag, `-listen-addr`,
which wins. One block at most:

```hcl
listen {
  addr                = ":8080"
  read_header_timeout = "10s"
  read_timeout        = "1m"
  write_timeout       = "1m"
  idle_timeout        = "2m"
}
```

| Attribute | Default | Meaning |
|---|---|---|
| `addr` | `:8080` | Address of the dashboard and the git webhook. |
| `read_header_timeout` | `10s` | How long a client has to send the request headers. |
| `read_timeout` | `1m` | How long a client has to send the whole request. |
| `write_timeout` | `1m` | How long Nops has to write the response. |
| `idle_timeout` | `2m` | How long an idle connection stays open. |

A [backup](running-nops.md#backups) download isn't cut while it moves: `write_timeout`
limits each block it writes, not the whole file.

#### Certificate and key

A `tls` block makes Nops serve the dashboard and the git webhook over HTTPS
itself, either with no proxy or behind one that trusts your certificate authority:

```hcl
listen {
  tls {
    cert_file = "/secrets/nops.crt"
    key_file  = "/secrets/nops.key"
  }
}
```

| Attribute | Default | Meaning |
|---|---|---|
| `cert_file` | none, **required** | PEM file of the certificate, with its chain. |
| `key_file` | none, **required** | PEM file of its private key. |

- `SIGHUP` reloads both files; the rest of the configuration needs a restart.
  If the reload fails, Nops keeps the current pair and logs an ERROR.
- Every answer carries `Strict-Transport-Security`, so a browser reaches the
  host only over HTTPS for a year.
- Without a public URL, the one derived from the listen address starts with
  `https://`.

## Dashboard

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen-addr` | `NOPS_LISTEN_ADDR` | `:8080` | Address of the dashboard and the git webhook. |
| `-public-url` | `NOPS_PUBLIC_URL` | derived from `-listen-addr` | URL people use to reach the dashboard, with its [sub path](running-nops.md#under-a-sub-path) if any. Set it behind a proxy or with OIDC. |

Top-level attributes of the [configuration file](#configuration-file). Only
`public_url` has a flag, `-public-url`, which wins:

```hcl
public_url          = "https://nops.example.com"
webhook_secret_file = "/secrets/webhook"
```

| Attribute | Default | Meaning |
|---|---|---|
| `public_url` | derived from the listen address | URL people use to reach the dashboard. |
| `webhook_secret_file` | none | File holding the [git webhook](running-nops.md#the-git-webhook) secret. Unset: no webhook. |

## Authentication

The `auth` block of the [configuration file](#configuration-file) picks how
the dashboard logs people in ([authentication](dashboard.md#authentication)).
It has no flag or variable, and Nops needs exactly one:

```hcl
auth "authentik" {
  type               = "oidc"
  issuer_url         = "https://auth.example.com/application/o/nops/"
  client_id          = "nops"
  client_secret_file = "/secrets/oidc-secret"
}
```

| Attribute | Types | Default | Meaning |
|---|---|---|---|
| `type` | all | none, **required** | `oidc` or `basic`. |
| `issuer_url` | `oidc` | none, **required** | Issuer URL, exactly as the provider announces it, trailing slash included. |
| `client_id` | `oidc` | none, **required** | Client ID of Nops at the provider. |
| `client_secret_file` | `oidc` | none, **required** | File holding the client secret. |
| `users_file` | `basic` | none, **required** | File of [`username:bcrypt-hash` lines](dashboard.md#local-users). |
| `groups_file` | `basic` | none | File of [`group: user1 user2` lines](dashboard.md#local-users). |
| `max_failures` | `basic` | `10` | Failed logins of a username before it's locked. `0`: no limit. |
| `failure_window` | `basic` | `15m` | How long a username stays locked, counted from its first failure. `0`: no limit. |

- The name, made of letters, digits, `-` and `_`, is how
  [binding rules](acl.md#binding-rules) name the auth method.
- A second block, or an attribute of the other type, stops Nops.

## Access control

The `acl` block of the [configuration file](#configuration-file) turns on
the [ACL](acl.md). It has no flag or variable, and one block at most:

```hcl
acl {
  enabled              = true
  bootstrap_token_file = "/secrets/bootstrap-token"
}
```

| Attribute | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Turn on the ACL. Off, every token can do everything. |
| `bootstrap_token_file` | none, **required** with `enabled` | File holding the [bootstrap token](acl.md#tokens), made by `nops secret generate`. |

## Notifications

Each `notify` block of the [configuration file](#configuration-file) turns
on a [notification adapter](logs-and-notifications.md#notifications). It has
no flag or variable. You can turn on several, of the same type too:

```hcl
notify "slack" {
  type     = "slack"
  url_file = "/secrets/slack-url"
}

notify "alerts" {
  type       = "ntfy"
  url        = "https://ntfy.example.com/nops"
  token_file = "/secrets/ntfy-token"
}
```

| Attribute | Types | Default | Meaning |
|---|---|---|---|
| `type` | all | none, **required** | `webhook`, `discord`, `slack`, `ntfy`, or `gotify`. |
| `url_file` | `webhook`, `discord`, `slack` | none, **required** | File holding the URL: the receiver of the generic JSON, the Discord webhook, or the Slack incoming webhook. |
| `url` | `ntfy`, `gotify` | none, **required** | ntfy topic URL, or Gotify server URL. |
| `token_file` | `webhook`, `ntfy`, `gotify` | none, **required** for `gotify` | File holding the token: a bearer token for the webhook and ntfy, the app token for Gotify. |
| `timeout` | all | `10s` | Timeout of one notification request. |

- The name, made of letters, digits, `-` and `_`, labels the adapter in the
  log and in [metrics](metrics.md).
- A name used twice, or an attribute of another type, stops Nops.

## Storage, intervals, and timeouts

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-db-path` | `NOPS_DB_PATH` | `nops.db` | SQLite database file. Keep it on persistent storage. |
| `-log-level` | `NOPS_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

Top-level attributes of the [configuration file](#configuration-file). Only
`db_path` and `log_level` have a flag, which wins:

```hcl
db_path           = "/data/nops.db"
git_poll_interval = "30s"
apply_timeout     = "15m"
```

| Attribute | Default | Meaning |
|---|---|---|
| `db_path` | `nops.db` | SQLite database file. |
| `spec_retention` | `720h` | How long a deployment that has ended keeps the spec, the job file and var-file, and its hooks' specs. The latest completed deployment of each job keeps them. `0`: forever. |
| `log_level` | `info` | `debug`, `info`, `warn` or `error`. |
| `git_poll_interval` | `1m` | How often Nops polls each repository. |
| `drift_interval` | `5m` | How often Nops checks Nomad for drift without a new commit. |
| `engine_interval` | `5s` | How often the engine advances active deployments. |
| `hook_poll_interval` | `5s` | How often Nops checks a running hook. |
| `apply_timeout` | `10m` | How long an [apply](architecture.md#the-apply-timeout) may take. Covers the register and the wait for health. |
| `sync_window_time_zone` | `UTC` | IANA time zone of the [sync windows](policies.md#sync-window). |

A hook's timeout is set on the hook job with `nops_timeout`
([meta keys](meta-keys.md)).
