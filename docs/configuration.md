# Configuration

nops is configured with command-line flags or environment variables. The
source of truth is [`internal/config`](../internal/config/config.go): this
page and that package must be updated together (a test checks that every
option is listed here).

## Rules

- **Every option has a flag and a variable.** The variable is `NOPS_` followed
  by the flag name in upper case, with dashes as underscores:
  `-apply-timeout` → `NOPS_APPLY_TIMEOUT`.
- **Precedence: flag > variable > default.** A variable set to the empty
  string counts as unset. A flag set to the empty string (`-notify-ntfy-url=`) is a
  value and wins.
- **nops reads nothing else.** The standard `NOMAD_*` variables (`NOMAD_ADDR`,
  `NOMAD_TOKEN`, `NOMAD_NAMESPACE`, …) are **ignored**. When nops runs as a
  Nomad job, Nomad sets `NOMAD_NAMESPACE` and `NOMAD_REGION` in its
  environment (and `NOMAD_TOKEN` with `identity { env = true }`). With the
  Nomad client's defaults nops would then work on the namespace it runs in,
  not on the one you configured.
- **Secrets are files, by flag.** Every secret has a `-X-file` flag (and its
  `NOPS_X_FILE` variable) that reads it from a file: tokens, and URLs that
  carry one (Discord and Slack webhooks), never appear in the command line
  (`ps`, `/proc/<pid>/cmdline`) or in the `args` of a job spec that way. The
  file is read once at startup and surrounding whitespace is trimmed. A path
  that cannot be read, or a file that is empty, is an error. An error about a
  URL read from a file names the file, never the URL.
- **Secrets without a file.** A flag is the only place a secret must not sit
  in the clear: `ps` shows every process's arguments to any local user. An
  **environment variable does not have that problem** — it is only readable
  from `/proc/<pid>/environ`, by the same user or root, exactly like a file
  with `0600` permissions. So every secret above also has a second, env-only
  variable with the literal value, named like its `_FILE` variable with
  `_FILE` dropped (`NOPS_GIT_TOKEN_FILE` → `NOPS_GIT_TOKEN`,
  `NOPS_NOTIFY_DISCORD_URL_FILE` → `NOPS_NOTIFY_DISCORD_URL`, …). There is
  **no flag** for it. Precedence: **flag file > variable file > this
  variable > off**. It suits an operator whose deployment tool already
  injects secrets into the environment safely (systemd `EnvironmentFile`,
  `docker run --env-file`, a Kubernetes `secretKeyRef`, a Nomad `template`
  block with `env = true`) without a temporary file. Whether a literal secret
  ends up in a checked-in Nomad job's `env {}` block instead of coming from
  Vault or a Nomad Variable is the operator's call: nops cannot and does not
  try to prevent it.
- **Every error is reported at once**, each one prefixed with where the value
  came from (`flag -apply-timeout: ...`, `NOPS_APPLY_TIMEOUT: ...`, or
  `-git-url / NOPS_GIT_URL: required` when neither was set).
- Durations use Go syntax (`90s`, `5m`, `1h`) and must be positive.
- `nops -h` prints every flag with its variable and default.

### Secrets without a file, at a glance

| `_FILE` variable (and its flag) | Plain variable, env-only |
|---|---|
| `NOPS_GIT_TOKEN_FILE` (`-git-token-file`) | `NOPS_GIT_TOKEN` |
| `NOPS_NOMAD_TOKEN_FILE` (`-nomad-token-file`) | `NOPS_NOMAD_TOKEN` |
| `NOPS_OIDC_CLIENT_SECRET_FILE` (`-oidc-client-secret-file`) | `NOPS_OIDC_CLIENT_SECRET` |
| `NOPS_WEBHOOK_SECRET_FILE` (`-webhook-secret-file`) | `NOPS_WEBHOOK_SECRET` |
| `NOPS_NOTIFY_WEBHOOK_URL_FILE` (`-notify-webhook-url-file`) | `NOPS_NOTIFY_WEBHOOK_URL` |
| `NOPS_NOTIFY_WEBHOOK_TOKEN_FILE` (`-notify-webhook-token-file`) | `NOPS_NOTIFY_WEBHOOK_TOKEN` |
| `NOPS_NOTIFY_DISCORD_URL_FILE` (`-notify-discord-url-file`) | `NOPS_NOTIFY_DISCORD_URL` |
| `NOPS_NOTIFY_SLACK_URL_FILE` (`-notify-slack-url-file`) | `NOPS_NOTIFY_SLACK_URL` |
| `NOPS_NOTIFY_NTFY_TOKEN_FILE` (`-notify-ntfy-token-file`) | `NOPS_NOTIFY_NTFY_TOKEN` |
| `NOPS_NOTIFY_GOTIFY_TOKEN_FILE` (`-notify-gotify-token-file`) | `NOPS_NOTIFY_GOTIFY_TOKEN` |

## Nomad

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-nomad-addr` | `NOPS_NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad HTTP API address (`http://` or `https://`). |
| `-nomad-namespace` | `NOPS_NOMAD_NAMESPACE` | `default` | Namespace of the managed jobs and of the hook jobs. |
| `-nomad-token-file` | `NOPS_NOMAD_TOKEN_FILE` | none | File holding the Nomad ACL token. Unset: no token (ACLs disabled). |
| `-nomad-ca-cert` | `NOPS_NOMAD_CA_CERT` | none | PEM file of the CA that signed the Nomad server certificate. |
| `-nomad-client-cert` | `NOPS_NOMAD_CLIENT_CERT` | none | PEM client certificate for mTLS. Set together with the key. |
| `-nomad-client-key` | `NOPS_NOMAD_CLIENT_KEY` | none | PEM client key for mTLS. Set together with the certificate. |
| `-nomad-tls-skip-verify` | `NOPS_NOMAD_TLS_SKIP_VERIFY` | `false` | Do not verify the server certificate. Insecure: for testing only. |

## Git

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-git-url` | `NOPS_GIT_URL` | **required** | URL of the repository holding the jobs. |
| `-git-branch` | `NOPS_GIT_BRANCH` | `main` | Branch to read. |
| `-git-path` | `NOPS_GIT_PATH` | none | Subdirectory of the repository holding the job files. Relative, no `..`. Unset: the repository root. |
| `-git-username` | `NOPS_GIT_USERNAME` | `git` | Username sent with the token (GitHub ignores it; GitLab wants `oauth2`, Gitea the account name). |
| `-git-token-file` | `NOPS_GIT_TOKEN_FILE` | none | File holding the HTTPS token. Unset: public repository. Requires an `https://` URL. |

A private repository is read over HTTPS with a token (basic auth). SSH is not
supported. Which files under `-git-path` are read as jobs is described in
[gitwatch](design/gitwatch.md#which-files-are-read).

## Dashboard

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen-addr` | `NOPS_LISTEN_ADDR` | `:8080` | Address of the dashboard and of the git webhook (`host:port`). |
| `-webhook-secret-file` | `NOPS_WEBHOOK_SECRET_FILE` | none | File holding the git forge's webhook secret (or `NOPS_WEBHOOK_SECRET`, see [secrets without a file](#secrets-without-a-file-at-a-glance)). Unset: `/webhook/git` answers 404. See [dashboard](dashboard.md#git-webhook). |
| `-public-url` | `NOPS_PUBLIC_URL` | derived from `-listen-addr` | The URL people use to reach the dashboard, e.g. `https://nops.example.com` (nops sits behind a proxy and cannot know it). Used to build the OIDC redirect URL, to decide whether session cookies are `Secure`, and notifications link to `<public-url>/deployments/<id>`. Left unset, it becomes `http://<host>:<port>` from `-listen-addr`, `localhost` in place of an empty or wildcard host (`0.0.0.0`, `::`) — meant for `-auth-mode=basic` with no reverse proxy; with `-auth-mode=oidc` set it explicitly to what the browser and the provider's registered redirect URI actually need, since `localhost` is essentially never that. |
| `-auth-mode` | `NOPS_AUTH_MODE` | none, **required** | How the dashboard logs people in: `oidc` or `basic`. The two are mutually exclusive — only the options of the chosen one may be set, nops refuses to start otherwise (a leftover flag from switching modes is caught, not silently ignored). See [authentication](dashboard.md#authentication). |

### OIDC (`-auth-mode=oidc`)

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-oidc-issuer-url` | `NOPS_OIDC_ISSUER_URL` | none, **required** | Issuer URL of the OIDC provider, exactly as it announces it in its discovery document: a trailing slash matters (Authentik's has one), nops does not add or drop it. |
| `-oidc-client-id` | `NOPS_OIDC_CLIENT_ID` | none, **required** | Client ID of nops at the provider. |
| `-oidc-client-secret-file` | `NOPS_OIDC_CLIENT_SECRET_FILE` | none, **required** | File holding the client secret (or `NOPS_OIDC_CLIENT_SECRET`, see [secrets without a file](#secrets-without-a-file-at-a-glance)). |
| `-oidc-allowed-users` | `NOPS_OIDC_ALLOWED_USERS` | none | Comma-separated usernames (`preferred_username`) or emails that may log in. |
| `-oidc-allowed-groups` | `NOPS_OIDC_ALLOWED_GROUPS` | none | Comma-separated groups (the `groups` claim) that may log in. |

At least one of the two allowlists must be set: with both empty nops does not
start, since every user of the provider would be able to approve a
deployment. A user must match either list. How the login works, and how to
set the client up at Authentik or Authelia, is in
[dashboard](dashboard.md#authentication).

### Local users (`-auth-mode=basic`)

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-users-file` | `NOPS_USERS_FILE` | none, **required** | File holding one `username:bcrypt-hash` per line (blank lines and `#` comments ignored), read once at startup. Generate a line with `htpasswd -nB <user>`. See [dashboard](dashboard.md#local-users). |

No OIDC provider needed: nops checks the password itself against the file.
The actor recorded with a decision is the username as is. There is no
lockout or rate limiting on failed attempts — a known limitation for this
personal-use tool, see [dashboard](dashboard.md#local-users).

## Notifications

Each [notification adapter](error-handling.md#notifications) has its own
options and is on when its URL is set. Several can be on at once: each
receives every notification. None set: notifications are off.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-notify-webhook-url-file` | `NOPS_NOTIFY_WEBHOOK_URL_FILE` | none | File holding the URL that receives the generic JSON (n8n, Home Assistant, your own receiver). |
| `-notify-webhook-token-file` | `NOPS_NOTIFY_WEBHOOK_TOKEN_FILE` | none | File holding a token sent as `Authorization: Bearer`. Needs the URL. |
| `-notify-discord-url-file` | `NOPS_NOTIFY_DISCORD_URL_FILE` | none | File holding the Discord webhook URL (channel settings → Integrations → Webhooks). |
| `-notify-slack-url-file` | `NOPS_NOTIFY_SLACK_URL_FILE` | none | File holding the Slack incoming webhook URL (`https://hooks.slack.com/services/...`). |
| `-notify-ntfy-url` | `NOPS_NOTIFY_NTFY_URL` | none | ntfy topic URL, e.g. `https://ntfy.example.com/nops`. |
| `-notify-ntfy-token-file` | `NOPS_NOTIFY_NTFY_TOKEN_FILE` | none | File holding an ntfy access token (`tk_...`). Needs the URL. |
| `-notify-gotify-url` | `NOPS_NOTIFY_GOTIFY_URL` | none | Gotify server URL, e.g. `https://gotify.example.com`; nops posts to `<url>/message`. |
| `-notify-gotify-token-file` | `NOPS_NOTIFY_GOTIFY_TOKEN_FILE` | none | File holding the Gotify application token. **Required** with the URL. |
| `-notify-timeout` | `NOPS_NOTIFY_TIMEOUT` | `10s` | Timeout of one notification request, for every adapter. |

The Discord and Slack URLs contain their token, so they are files like the
other secrets. The ntfy and Gotify URLs are plain server addresses and their
tokens are files. On the public `ntfy.sh` anyone who knows the topic can read
it, so use a topic nobody can guess, or your own server with a token.

## Storage, intervals and timeouts

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-db-path` | `NOPS_DB_PATH` | `nops.db` | SQLite database file. Put it on persistent storage. |
| `-git-poll-interval` | `NOPS_GIT_POLL_INTERVAL` | `1m` | How often the repository is fetched. The git webhook triggers a fetch sooner. |
| `-drift-interval` | `NOPS_DRIFT_INTERVAL` | `5m` | How often Nomad is checked for drift when there is no new commit. |
| `-engine-interval` | `NOPS_ENGINE_INTERVAL` | `5s` | How often the engine advances active deployments. |
| `-hook-poll-interval` | `NOPS_HOOK_POLL_INTERVAL` | `5s` | How often a running hook is checked. |
| `-apply-timeout` | `NOPS_APPLY_TIMEOUT` | `10m` | How long an apply may wait for the Nomad deployment to be `successful` (see [architecture](architecture.md#apply-and-downtime)). |
| `-log-level` | `NOPS_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

The timeout of a hook is not here: it is set per job with
`nops_pre_hook_timeout` / `nops_post_hook_timeout` (see
[meta keys](meta-keys.md)).

## Running nops as a Nomad job

The tokens come from Nomad Variables through a `template`, and the variables
point at the rendered files:

```hcl
task "nops" {
  template {
    destination = "secrets/git-token"
    data        = "{{ with nomadVar \"nomad/jobs/nops\" }}{{ .git_token }}{{ end }}"
    change_mode = "restart"
  }

  template {
    destination = "secrets/discord-url"
    data        = "{{ with nomadVar \"nomad/jobs/nops\" }}{{ .discord_url }}{{ end }}"
    change_mode = "restart"
  }

  template {
    destination = "secrets/oidc-secret"
    data        = "{{ with nomadVar \"nomad/jobs/nops\" }}{{ .oidc_client_secret }}{{ end }}"
    change_mode = "restart"
  }

  env {
    NOPS_GIT_URL                 = "https://git.example.com/ops/jobs.git"
    NOPS_GIT_TOKEN_FILE          = "${NOMAD_SECRETS_DIR}/git-token"
    NOPS_NOTIFY_DISCORD_URL_FILE = "${NOMAD_SECRETS_DIR}/discord-url"
    NOPS_PUBLIC_URL              = "https://nops.example.com"
    NOPS_OIDC_ISSUER_URL         = "https://auth.example.com/application/o/nops/"
    NOPS_OIDC_CLIENT_ID          = "nops"
    NOPS_OIDC_CLIENT_SECRET_FILE = "${NOMAD_SECRETS_DIR}/oidc-secret"
    NOPS_OIDC_ALLOWED_GROUPS     = "nops-approvers"
    NOPS_NOMAD_ADDR              = "https://nomad.service.consul:4646"
    NOPS_DB_PATH                 = "/data/nops.db"
  }
}
```

`change_mode = "restart"` restarts nops when a secret changes, since secrets
are read only at startup.
