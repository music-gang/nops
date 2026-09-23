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
  string counts as unset. A flag set to the empty string (`-notify-url=`) is a
  value and wins.
- **nops reads nothing else.** The standard `NOMAD_*` variables (`NOMAD_ADDR`,
  `NOMAD_TOKEN`, `NOMAD_NAMESPACE`, …) are **ignored**. When nops runs as a
  Nomad job, Nomad sets `NOMAD_NAMESPACE` and `NOMAD_REGION` in its
  environment (and `NOMAD_TOKEN` with `identity { env = true }`). With the
  Nomad client's defaults nops would then work on the namespace it runs in,
  not on the one you configured.
- **Secrets are files.** Tokens are passed as the path of a file that holds
  them (`-git-token-file`, `-nomad-token-file`). That way they never appear in
  the command line (`ps`, `/proc/<pid>/cmdline`) or in the `args` of the job
  spec. The file is read once at startup and surrounding whitespace is
  trimmed. A path that cannot be read, or a file that is empty, is an error.
- **Every error is reported at once**, each one prefixed with where the value
  came from (`flag -apply-timeout: ...`, `NOPS_APPLY_TIMEOUT: ...`, or
  `-git-url / NOPS_GIT_URL: required` when neither was set).
- Durations use Go syntax (`90s`, `5m`, `1h`) and must be positive.
- `nops -h` prints every flag with its variable and default.

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
| `-git-username` | `NOPS_GIT_USERNAME` | `git` | Username sent with the token (GitHub ignores it; GitLab wants `oauth2`, Gitea the account name). |
| `-git-token-file` | `NOPS_GIT_TOKEN_FILE` | none | File holding the HTTPS token. Unset: public repository. Requires an `https://` URL. |

A private repository is read over HTTPS with a token (basic auth). SSH is not
supported.

## Dashboard and notifications

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen-addr` | `NOPS_LISTEN_ADDR` | `:8080` | Address of the dashboard and of the git webhook (`host:port`). |
| `-auth-header` | `NOPS_AUTH_HEADER` | `Remote-User` | Request header carrying the user authenticated by the reverse proxy (see [dashboard](dashboard.md#authentication)). |
| `-notify-url` | `NOPS_NOTIFY_URL` | none | URL that receives [notifications](error-handling.md#notifications) as a JSON POST. Unset: notifications disabled. |
| `-notify-timeout` | `NOPS_NOTIFY_TIMEOUT` | `10s` | Timeout of one notification request. |

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

  env {
    NOPS_GIT_URL        = "https://git.example.com/ops/jobs.git"
    NOPS_GIT_TOKEN_FILE = "${NOMAD_SECRETS_DIR}/git-token"
    NOPS_NOMAD_ADDR     = "https://nomad.service.consul:4646"
    NOPS_DB_PATH        = "/data/nops.db"
  }
}
```

`change_mode = "restart"` restarts nops when the token changes, since it is
read only at startup.
