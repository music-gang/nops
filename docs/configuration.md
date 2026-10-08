# Configuration

Every option of the Nops server, `nops serve`. To run Nops on a cluster, see
[running Nops](running-nops.md). The options of the client are in
[command-line client](cli.md).

## Rules

- Every option has a flag and an environment variable: `NOPS_` plus the flag
  name in upper case, dashes as underscores (`-apply-timeout` →
  `NOPS_APPLY_TIMEOUT`).
- Precedence: flag, then variable, then default. An empty variable counts as
  unset.
- Nops ignores the standard `NOMAD_*` variables, so the ones Nomad sets for a
  job don't change what Nops manages.
- Durations use Go syntax (`90s`, `5m`, `1h`) and must be positive.
- Nops reports every invalid option at once and refuses to start.
- `nops serve -h` lists every flag. `nops -version` prints the version.

### Secrets

Every secret is read from a file, through a `-X-file` flag or its
`NOPS_X_FILE` variable, so it never shows up in `ps`. You can also pass the
value itself in an environment variable without `_FILE`, for tools that
inject secrets into the environment. These variables have no flag.

| From a file (flag) | Value in the environment |
|---|---|
| `NOPS_GIT_TOKEN_FILE` (`-git-token-file`) | `NOPS_GIT_TOKEN` |
| `NOPS_NOMAD_TOKEN_FILE` (`-nomad-token-file`) | `NOPS_NOMAD_TOKEN` |
| `NOPS_ACL_BOOTSTRAP_TOKEN_FILE` (`-acl-bootstrap-token-file`) | `NOPS_ACL_BOOTSTRAP_TOKEN` |
| `NOPS_OIDC_CLIENT_SECRET_FILE` (`-oidc-client-secret-file`) | `NOPS_OIDC_CLIENT_SECRET` |
| `NOPS_WEBHOOK_SECRET_FILE` (`-webhook-secret-file`) | `NOPS_WEBHOOK_SECRET` |
| `NOPS_METRICS_TOKEN_FILE` (`-metrics-token-file`) | `NOPS_METRICS_TOKEN` |
| `NOPS_NOTIFY_WEBHOOK_URL_FILE` (`-notify-webhook-url-file`) | `NOPS_NOTIFY_WEBHOOK_URL` |
| `NOPS_NOTIFY_WEBHOOK_TOKEN_FILE` (`-notify-webhook-token-file`) | `NOPS_NOTIFY_WEBHOOK_TOKEN` |
| `NOPS_NOTIFY_DISCORD_URL_FILE` (`-notify-discord-url-file`) | `NOPS_NOTIFY_DISCORD_URL` |
| `NOPS_NOTIFY_SLACK_URL_FILE` (`-notify-slack-url-file`) | `NOPS_NOTIFY_SLACK_URL` |
| `NOPS_NOTIFY_NTFY_TOKEN_FILE` (`-notify-ntfy-token-file`) | `NOPS_NOTIFY_NTFY_TOKEN` |
| `NOPS_NOTIFY_GOTIFY_TOKEN_FILE` (`-notify-gotify-token-file`) | `NOPS_NOTIFY_GOTIFY_TOKEN` |

## Nomad

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-nomad-addr` | `NOPS_NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad HTTP API address. |
| `-nomad-ui-url` | `NOPS_NOMAD_UI_URL` | none | Nomad UI address as a browser reaches it, for the dashboard's links. Unset: no links. |
| `-nomad-namespaces` | `NOPS_NOMAD_NAMESPACES` | `default` | Comma-separated [namespaces](running-nops.md#namespaces) Nops manages. |
| `-nomad-token-file` | `NOPS_NOMAD_TOKEN_FILE` | none | File holding the [Nomad ACL token](running-nops.md#the-nomad-token). |
| `-nomad-ca-cert` | `NOPS_NOMAD_CA_CERT` | none | PEM file of the CA of the Nomad server certificate. |
| `-nomad-client-cert` | `NOPS_NOMAD_CLIENT_CERT` | none | PEM client certificate for mTLS. |
| `-nomad-client-key` | `NOPS_NOMAD_CLIENT_KEY` | none | PEM client key for mTLS. |
| `-nomad-tls-skip-verify` | `NOPS_NOMAD_TLS_SKIP_VERIFY` | `false` | Skip server certificate checks. Testing only. |

## Git

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-git-url` | `NOPS_GIT_URL` | **required** | URL of the repository of jobs. |
| `-git-branch` | `NOPS_GIT_BRANCH` | `main` | Branch to read. |
| `-git-path` | `NOPS_GIT_PATH` | none | Subdirectory holding the job files. Unset: the root. |
| `-git-username` | `NOPS_GIT_USERNAME` | `git` | Username sent with the token: `oauth2` for GitLab, the user name for Gitea. |
| `-git-token-file` | `NOPS_GIT_TOKEN_FILE` | none | File holding the HTTPS token. Unset: public repository. |

Nops reads private repositories over HTTPS with a token. It doesn't support
SSH. The release image can't read `file://` URLs.

## Dashboard

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen-addr` | `NOPS_LISTEN_ADDR` | `:8080` | Address of the dashboard and the git webhook. |
| `-public-url` | `NOPS_PUBLIC_URL` | derived from `-listen-addr` | URL people use to reach the dashboard, with its [sub path](running-nops.md#under-a-sub-path) if any. Set it behind a proxy or with OIDC. |
| `-auth-mode` | `NOPS_AUTH_MODE` | none, **required** | `oidc` or `basic`: see [authentication](dashboard.md#authentication). |
| `-webhook-secret-file` | `NOPS_WEBHOOK_SECRET_FILE` | none | File holding the [git webhook](running-nops.md#the-git-webhook) secret. Unset: no webhook. |
| `-metrics-token-file` | `NOPS_METRICS_TOKEN_FILE` | none | File holding the bearer token [`/metrics`](metrics.md#scraping) requires. Unset: open. |

### OIDC (`-auth-mode=oidc`)

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-oidc-issuer-url` | `NOPS_OIDC_ISSUER_URL` | none, **required** | Issuer URL, exactly as the provider announces it, trailing slash included. |
| `-oidc-client-id` | `NOPS_OIDC_CLIENT_ID` | none, **required** | Client ID of Nops at the provider. |
| `-oidc-client-secret-file` | `NOPS_OIDC_CLIENT_SECRET_FILE` | none, **required** | File holding the client secret. |
| `-oidc-allowed-users` | `NOPS_OIDC_ALLOWED_USERS` | none | Comma-separated usernames or emails allowed to log in. |
| `-oidc-allowed-groups` | `NOPS_OIDC_ALLOWED_GROUPS` | none | Comma-separated groups allowed to log in. |

Set at least one allowlist.

### Local users (`-auth-mode=basic`)

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-users-file` | `NOPS_USERS_FILE` | none, **required** | File of [`username:bcrypt-hash` lines](dashboard.md#local-users--auth-modebasic). |

## Access control

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-acl` | `NOPS_ACL` | `false` | Turn on the [ACL](acl.md). Off, every token can do everything. |
| `-acl-bootstrap-token-file` | `NOPS_ACL_BOOTSTRAP_TOKEN_FILE` | none, **required** with `-acl` | File holding the [bootstrap token](acl.md#tokens), made by `nops secret generate`. |

## Notifications

Each [notification adapter](logs-and-notifications.md#notifications) turns on
when you set its URL. You can turn on several.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-notify-webhook-url-file` | `NOPS_NOTIFY_WEBHOOK_URL_FILE` | none | File holding the URL that receives the generic JSON. |
| `-notify-webhook-token-file` | `NOPS_NOTIFY_WEBHOOK_TOKEN_FILE` | none | File holding a bearer token for the generic webhook. |
| `-notify-discord-url-file` | `NOPS_NOTIFY_DISCORD_URL_FILE` | none | File holding the Discord webhook URL. |
| `-notify-slack-url-file` | `NOPS_NOTIFY_SLACK_URL_FILE` | none | File holding the Slack incoming webhook URL. |
| `-notify-ntfy-url` | `NOPS_NOTIFY_NTFY_URL` | none | ntfy topic URL. |
| `-notify-ntfy-token-file` | `NOPS_NOTIFY_NTFY_TOKEN_FILE` | none | File holding an ntfy access token. |
| `-notify-gotify-url` | `NOPS_NOTIFY_GOTIFY_URL` | none | Gotify server URL. |
| `-notify-gotify-token-file` | `NOPS_NOTIFY_GOTIFY_TOKEN_FILE` | none | File holding the Gotify app token. Required with the URL. |
| `-notify-timeout` | `NOPS_NOTIFY_TIMEOUT` | `10s` | Timeout of one notification request. |

## Storage, intervals, and timeouts

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-db-path` | `NOPS_DB_PATH` | `nops.db` | SQLite database file. Keep it on persistent storage. |
| `-git-poll-interval` | `NOPS_GIT_POLL_INTERVAL` | `1m` | How often Nops fetches the repository. |
| `-drift-interval` | `NOPS_DRIFT_INTERVAL` | `5m` | How often Nops checks Nomad for drift without a new commit. |
| `-engine-interval` | `NOPS_ENGINE_INTERVAL` | `5s` | How often the engine advances active deployments. |
| `-hook-poll-interval` | `NOPS_HOOK_POLL_INTERVAL` | `5s` | How often Nops checks a running hook. |
| `-apply-timeout` | `NOPS_APPLY_TIMEOUT` | `10m` | How long an [apply](architecture.md#the-apply-timeout) may take. Covers the register and the wait for health. |
| `-sync-window-time-zone` | `NOPS_SYNC_WINDOW_TIME_ZONE` | `UTC` | IANA time zone of the [sync windows](policies.md#sync-window). |
| `-log-level` | `NOPS_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

A hook's timeout is set on the hook job with `nops_timeout`
([meta keys](meta-keys.md)).
