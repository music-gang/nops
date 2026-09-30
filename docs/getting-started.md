# Getting started

In about fifteen minutes on one machine: run Nomad in development mode, point
Nops at a local git repository holding one of the [examples](../examples/),
approve its first deployment from the dashboard, then let a commit deploy on
its own.

## What you need

- **Linux with Docker running**: the example runs containers.
- **[Nomad](https://developer.hashicorp.com/nomad/install)**, the version in
  the README's [Nomad compatibility](../README.md#nomad-compatibility).
- **git**, and a clone of this repository for the example:
  `git clone https://github.com/music-gang/nops`.
- **Nops**: the `linux/amd64` binary from the
  [Releases](https://github.com/music-gang/nops/releases) page, or
  `go install github.com/music-gang/nops/cmd/nops@latest`. Not the container
  image for this guide: it has no `git`, which a `file://` repository needs.
- **`htpasswd`** (`apache2-utils` on Debian and Ubuntu), or Docker to run it.

## 1. Start Nomad

```sh
sudo nomad agent -dev -bind 0.0.0.0 -network-interface eth0
```

Replace `eth0` with your machine's main network interface
(`ip route show default` prints it after `dev`). Plain `nomad agent -dev`
gives every service the address `127.0.0.1`, which the example's smoke test,
running in a container of its own, cannot reach.

Leave it running. In another terminal, `nomad node status` shows one node,
`ready`.

## 2. Make a repository of jobs

```sh
mkdir nops-jobs
cp nops/examples/approval/* nops-jobs/
cd nops-jobs
git init -b main
git add .
git commit -m "web and its smoke test"
cd ..
```

It holds three files:

| File | What it is |
|---|---|
| `web.nomad.hcl` | A small web service. Its `meta` block makes Nops manage it (`nops_managed`), under the policy `approval` (`nops_policy`), with `web-smoke` run after each deployment (`nops_post_hook`). |
| `web-smoke.nomad.hcl` | The hook: a parameterized batch job that calls the service once it is up. |
| `web.vars.hcl` | The values of `web`'s variables: its image and how many instances. |

## 3. Make a user for the dashboard

```sh
htpasswd -nbB admin change-me > users
```

Without `apache2-utils`, the same with Docker:

```sh
docker run --rm httpd:2.4-alpine htpasswd -nbB admin change-me > users
```

## 4. Start Nops

```sh
nops -git-url "file://$PWD/nops-jobs" -auth-mode basic -users-file users -git-poll-interval 10s
```

Everything else is the default: Nomad at `http://127.0.0.1:4646`, the
dashboard on port 8080, the database in `nops.db` in the current directory.
`-git-poll-interval 10s` makes Nops notice a commit within seconds instead of
the default minute. Every option is in [configuration](configuration.md).

Nops logs one JSON line per event. Its first cycle finds that `web` is in git
but not in Nomad, and logs `deployment pending_approval` for it.

## 5. Approve the first deployment

Open <http://localhost:8080> and log in as `admin`, password `change-me`.

The Overview lists `web` as waiting for approval. Its page shows the
difference with the cluster (the whole job, since it does not exist yet) and
what *Approve* will do. Approve it.

Nops registers the job, waits for it to be healthy, then runs `web-smoke`.
Within a minute the deployment is **completed**, and the log shows each step:
`deployment approved`, `apply registered`, `deployment post_hook`,
`hook succeeded`, `deployment completed`. `nomad job status web` shows it
running.

## 6. Let a commit deploy on its own

In `nops-jobs`, set the policy to `auto` and ask for two instances:

- in `web.nomad.hcl`: `nops_policy = "auto"`;
- in `web.vars.hcl`: `count = 2`.

```sh
cd nops-jobs
git commit -am "web: deploy on its own, two instances"
cd ..
```

Within a few seconds Nops fetches the commit, plans it and applies it without
asking anyone, then runs the smoke test again. `nomad job status web` shows
two instances running.

## Clean up

Stop Nops with `Ctrl-C`, then remove the job and stop Nomad (in development
mode it keeps nothing):

```sh
nomad job stop -purge web
```

`Ctrl-C` in Nomad's terminal, and `rm nops.db users`.

## Next

- [Policies](policies.md): which policy suits which job, approving,
  sync windows, pausing a job.
- [Hooks](hooks.md): writing your own, and the other
  [examples](../examples/): a migration, a backup, pre-pulling an image.
- [Running Nops](running-nops.md): Nops on a real cluster, as a Nomad job.
- [Dashboard](dashboard.md): every page and what it shows.
