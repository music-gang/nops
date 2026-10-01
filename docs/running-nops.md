# Running Nops

Nops on a real cluster: where to get it, the Nomad job that runs it, the
token it needs, and what to set up around it (a reverse proxy, the login, the
git webhook). To try it on one machine first, see
[getting started](getting-started.md); every option is in
[configuration](configuration.md).

## Getting it

Each release publishes an image and a `linux/amd64` binary:

```sh
docker run --rm ghcr.io/music-gang/nops:0.4 -version
```

The binary, with its checksums, is on the
[Releases](https://github.com/music-gang/nops/releases) page, and
`go install github.com/music-gang/nops/cmd/nops@<version>` builds it. The
image's tags for a release `vX.Y.Z` are `X.Y.Z`, `X.Y` and `latest` (and,
from `v1`, `X`); a release candidate gets only its exact tag. The image holds
the binary and nothing else: no shell, and no `git`, so a `file://`
repository URL does not work in it.

## As a Nomad job

The image runs as the `nonroot` user (uid 65532), so the volume holding the
database must be writable by that uid. Pin its version as
[Upgrading](#upgrading) says.

A group running it. The tokens come from Nomad Variables through a
`template`, and the variables point at the rendered files:

```hcl
group "nops" {
  network {
    port "http" { to = 8080 }
  }

  # A host volume (or a CSI one) for the SQLite database and its -wal/-shm
  # files, writable by uid 65532.
  volume "nops-data" {
    type   = "host"
    source = "nops-data"
  }

  service {
    name = "nops"
    port = "http"
    check {
      type     = "http"
      path     = "/healthz"
      interval = "10s"
      timeout  = "2s"
    }
  }

  task "nops" {
    driver = "docker"

    config {
      image = "ghcr.io/music-gang/nops:0.1"
      ports = ["http"]
    }

    volume_mount {
      volume      = "nops-data"
      destination = "/data"
    }

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
}
```

`change_mode = "restart"` restarts Nops when a secret changes, since secrets
are read only at startup.

## Namespaces

One Nops instance manages every namespace of `-nomad-namespaces`, with one
token. A job belongs to the namespace its HCL declares (`namespace = "apps"`;
Nomad makes it `default` when there is none), and a hook is looked up in the
namespace of the job that declares it ([hooks](hooks.md#namespace)). A job in a
namespace that is not on the list is an ERROR in the log and is not managed;
so a job with no `namespace` is refused unless `default` is listed.

The list is explicit on purpose: the token's ACL says what Nops *can* do, the
list says what it *may* do, and pushing to git must not be enough to reach a
namespace nobody named. Taking a namespace off the list leaves its deployments
and its jobs in Nomad as they are: Nops stops looking at them, it does not
clean up.

The old `-nomad-namespace` / `NOPS_NOMAD_NAMESPACE` no longer exists: setting
it is an error, not a silent default.

## The Nomad token

The token needs, in each listed namespace, `read-job`, `list-jobs`,
`submit-job` (parse, plan, register and the deregister of hook revisions) and
`dispatch-job` (hooks). Nomad checks the parse of a job in the namespace the
request is made in, not in the job's: Nops asks in the first listed namespace,
so a token needs no rule in `default` unless `default` is listed
(`TestParseNeedsNoRuleInDefault`). On top of that, every **volume** a job or a hook mounts
needs a rule of its own, because Nomad checks volumes when a job is registered:

| The job declares | The token needs |
|---|---|
| a host volume, read-write (no `read_only`) | `host_volume "<source>"` with `policy = "write"` |
| a host volume with `read_only = true` | `host_volume "<source>"` with `policy = "read"` (`write` also works) |
| a CSI volume | `csi-mount-volume` in the namespace **and** `plugin { policy = "read" }`: either alone is refused |
| a task with `csi_plugin` | `csi-register-plugin` in the namespace |

`policy = "read"` grants `mount-readonly` only, `write` grants both mounts, so a
hook that mounts read-only a volume the token can already write needs no rule of
its own. The rule is per volume: a token without it is refused at register with
`register job <id>: Unexpected response code: 403 (Permission denied)`, while
`Plan` passes, since it checks no volume. The deployment stays `applying` until
`-apply-timeout`, then fails ([architecture](architecture.md#apply)).
`TestTokenACLForVolumes` checks every row of the table, and that `Plan` passes
without them.

```hcl
namespace "default" {
  capabilities = ["list-jobs", "read-job", "submit-job", "dispatch-job"]
}

host_volume "db-data" {
  policy = "write"
}
```

A name in a policy may hold only letters, digits, `-` and `*`, so a volume
declared as `nomad_backup_data` cannot be written as it is. Match it with a
glob (`host_volume "nomad*backup*data"`: `*` stands for `_` too) or name the
volume with `-` in the client configuration, the jobs and the policy
(`TestACLPolicyVolumeNames`).
The dashboard's [Nomad panel](dashboard.md#the-nomad-panel) reads with
`list-jobs` and `read-job` (`TestNomadPanelReadsWithReadJob`), and its *Promote*
button, which promotes the canaries of a Nomad deployment, needs `submit-job`:
a token with `list-jobs`, `read-job` and `submit-job` (and no `dispatch-job`)
promotes, and one with `list-jobs` and `read-job` only is refused with `403
(Permission denied)` (`TestPromoteNeedsSubmitJob`). Without it the button fails, the request stays on the
deployment's timeline, and the canaries stay unpromoted.
See Nomad's [ACL policy reference](https://developer.hashicorp.com/nomad/docs/secure/acl/policies).

## Under a sub path

Nops can be served under a sub path of a shared domain
(`https://domain.example.org/nops`) instead of a dedicated one: give
`-public-url` a path and that becomes the dashboard's base path
([configuration](configuration.md#dashboard)) — there is no separate flag,
since the browser-facing URL is the only place that matters.

The reverse proxy in front of Nops must forward the request path
**unstripped**, prefix included (`https://domain.example.org/nops/jobs`
must reach Nops as `/nops/jobs`, not `/jobs`): Nops does its own stripping,
once, at the edge (`http.StripPrefix`), and prepends the base path back to
every URL it generates itself — redirects, the session and login cookies'
`Path`, every link and form action a page renders, and the static assets —
so the browser and the proxy always see the full, prefixed address. Getting
this backwards (a proxy that already strips the prefix) makes every link
Nops renders double it.

`/healthz` and `/metrics` are the exceptions: they answer at the bare path
too, without the base path, since an orchestrator's health check (like the
[example job](#as-a-nomad-job)'s) and a Prometheus scrape hit the task's own
port directly, bypassing whatever prefix a reverse proxy mounts the dashboard
under. See the [decision log](archive/decisions.md), 2026-09-28.

## Logging in with OIDC

How the login works is in [dashboard](dashboard.md#authentication). To set
it up:

`-public-url` is no longer required to start Nops (see
[configuration](configuration.md#dashboard)): unset, it defaults to a
`localhost` guess derived from `-listen-addr`, which is essentially never
right for OIDC — a provider redirects the browser to the registered URI, not
to wherever Nops happens to be listening. **Set `-public-url` explicitly**
before configuring the client below.

Create an OIDC client (confidential, authorization code) at the provider:

- **Redirect URI:** `<public-url>/auth/callback`, e.g.
  `https://nops.example.com/auth/callback`.
- **Scopes:** `openid`, `profile`, `email`, and `groups` when you use
  `-oidc-allowed-groups` (Nops asks for `groups` only then). Check that the
  provider puts a `groups` claim in the ID token or in userinfo: with Authelia
  that is the `groups` scope, with Authentik the group mapping of the client.
- **Issuer:** the value in the provider's discovery document
  (`<issuer>/.well-known/openid-configuration`), trailing slash included:
  Authentik's is like `https://auth.example.com/application/o/nops/`.

## The git webhook

`POST /webhook/git` asks the git watcher for an out-of-turn poll
(`Watcher.Trigger()`, non-blocking: it does not wait for the poll to finish)
so a push shows up sooner than `-git-poll-interval`. It needs no session — it
is authenticated by a secret shared with the forge instead
(`-webhook-secret-file`, [configuration](configuration.md#dashboard)) — and
is disabled (404) when that secret is unset. Nops never looks at the payload
beyond checking its signature: any push is "something changed, go look", so
there is nothing forge-specific to parse.

Each forge signs differently, and Nops checks whichever header is present:

| Forge | Header | How it is checked |
|---|---|---|
| GitHub | `X-Hub-Signature-256: sha256=<hex>` | HMAC-SHA256 of the raw body with the secret, compared with `hmac.Equal`. |
| Gitea | `X-Gitea-Signature: <hex>` | Same construction, no `sha256=` prefix. |
| GitLab | `X-Gitlab-Token: <secret>` | The header must equal the secret itself, compared in constant time. |

A request matching none of them, or whose signature does not match, gets 401
and a WARN in the log that never says which check failed (so a probe cannot
learn which forge Nops expects). The body is capped at 1 MiB.

Set up the webhook at the forge: URL `<public-url>/webhook/git`, content
type `application/json`, secret the same file's content passed to
`-webhook-secret-file` (or `NOPS_WEBHOOK_SECRET`). Only a push to
`-git-branch` needs to trigger it, but Nops does not filter by branch or ref:
any authenticated request just triggers a poll, which is a no-op if nothing
under `-git-path` changed.

## Upgrading

- **Read the [release notes](https://github.com/music-gang/nops/releases)** of every version between
  yours and the new one. *Breaking changes* lists what may break, and a
  release that breaks something has *Upgrade notes* at the top saying what to
  change (an option, a meta key, the job, the database).
- **Pin a minor** (`0.4`) or an exact version (`0.4.1`) in the job, never
  `latest`: while the major is 0, a new minor may break the configuration or
  the database, and `latest` would take it on the next restart.
- A **release candidate** (`0.5.0-rc.1`) is published under its exact tag
  only, for trying a risky release before it becomes the minor.
