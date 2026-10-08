# Running Nops

How to run Nops on a cluster. To try it on one machine first, see
[getting started](getting-started.md).

## Getting it

Each release publishes a `linux/amd64` image, and binaries for Linux and
macOS on amd64 and arm64:

```sh
docker run --rm ghcr.io/music-gang/nops:0.7 -version
```

The binary is on the [Releases](https://github.com/music-gang/nops/releases)
page. Image tags for release `vX.Y.Z` are `X.Y.Z`, `X.Y` and `latest`.

## As a Nomad job

The image runs `nops serve`; a task that sets `args` starts them with
`"serve"`. It runs as uid 65532: make the database volume writable by that
user. This group reads its secrets from Nomad Variables:

```hcl
group "nops" {
  network {
    port "http" { to = 8080 }
  }

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
      image = "ghcr.io/music-gang/nops:0.7"
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
      destination = "secrets/oidc-secret"
      data        = "{{ with nomadVar \"nomad/jobs/nops\" }}{{ .oidc_client_secret }}{{ end }}"
      change_mode = "restart"
    }

    env {
      NOPS_GIT_URL                 = "https://git.example.com/ops/jobs.git"
      NOPS_GIT_TOKEN_FILE          = "${NOMAD_SECRETS_DIR}/git-token"
      NOPS_PUBLIC_URL              = "https://nops.example.com"
      NOPS_AUTH_MODE               = "oidc"
      NOPS_OIDC_ISSUER_URL         = "https://auth.example.com/application/o/nops/"
      NOPS_OIDC_CLIENT_ID          = "nops"
      NOPS_OIDC_CLIENT_SECRET_FILE = "${NOMAD_SECRETS_DIR}/oidc-secret"
      NOPS_NOMAD_ADDR              = "https://nomad.service.consul:4646"
      NOPS_DB_PATH                 = "/data/nops.db"
    }
  }
}
```

## Namespaces

List the namespaces Nops manages in `-nomad-namespaces`. Nops ignores jobs in
other namespaces and logs an ERROR for each. A job without a `namespace` is in
`default`. Removing a namespace from the list leaves its jobs running as they
are.

## The Nomad token

In each managed namespace, the token needs `list-jobs`, `read-job`,
`submit-job` and `dispatch-job`:

```hcl
namespace "default" {
  capabilities = ["list-jobs", "read-job", "submit-job", "dispatch-job"]
}
```

Nomad also checks every volume a job or hook mounts, so the token needs a rule
for each:

| The job mounts | The token needs |
|---|---|
| a host volume, read-write | `host_volume "<source>"` with `policy = "write"` |
| a host volume with `read_only = true` | `host_volume "<source>"` with `policy = "read"` |
| a CSI volume | `csi-mount-volume` in the namespace and `plugin { policy = "read" }` |
| a task with `csi_plugin` | `csi-register-plugin` in the namespace |

Without the rule, the register fails with a 403 and the deployment fails at
`-apply-timeout`. Policy names allow only letters, digits, `-` and `*`, so
match a volume like `nomad_backup_data` with `host_volume "nomad*backup*data"`.
See Nomad's [ACL policy reference](https://developer.hashicorp.com/nomad/docs/secure/acl/policies).

## Under a sub path

To serve Nops at `https://example.org/nops`, put the path in `-public-url`.
Your reverse proxy must forward the path as is, without stripping `/nops`.
`/healthz` and `/metrics` also answer at the bare path, for health checks and
scrapes that bypass the proxy.

## Logging in with OIDC

Set `-public-url`, then create a confidential OIDC client with the
authorization code flow at your provider:

- **Redirect URI:** `<public-url>/auth/callback`.
- **Scopes:** `openid`, `profile`, `email` and `groups`. A
  [binding rule](acl.md#binding-rules) on groups needs the provider to send a
  `groups` claim.
- **Issuer:** the `issuer` of the provider's
  `/.well-known/openid-configuration`, trailing slash included.

## The git webhook

The webhook makes Nops fetch git right after a push, instead of at the next
poll. Generate a random secret into a file, pass it with
`-webhook-secret-file`, then add a webhook at your forge:

```sh
openssl rand -hex 32 > nops-webhook-secret
```

- **URL:** `<public-url>/webhook/git`
- **Media type:** `application/json`
- **Secret:** the content of the secret file.

Nops checks GitHub, Gitea, and GitLab signatures, and answers 401 to
anything else.

## Upgrading

- Read the [release notes](https://github.com/music-gang/nops/releases) of every
  version between yours and the new one. A release that breaks something
  says what to change in its *Upgrade notes*.
- Pin a minor (`X.Y`) or an exact version, never `latest`: before `v1`, a new
  minor may break the configuration or the database.
