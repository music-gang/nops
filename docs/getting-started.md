# Getting started

In this tutorial you run Nomad and Nops on one machine, approve a first
deployment from the dashboard, then let a commit deploy on its own. It takes
about fifteen minutes.

## What you need

- **Linux with Docker running.**
- **[Nomad](https://developer.hashicorp.com/nomad/install)**, the version in
  the README's [Nomad compatibility](../README.md#nomad-compatibility).
- **git**, and a clone of this repository for the example:
  `git clone https://github.com/music-gang/nops`.
- **Nops**: the `linux/amd64` binary from the
  [Releases](https://github.com/music-gang/nops/releases) page, or
  `go install github.com/music-gang/nops/cmd/nops@latest`. The container
  image can't read the local repository this tutorial uses.
- **`htpasswd`** (`apache2-utils` on Debian and Ubuntu), or Docker to run it.

## 1. Start Nomad

```sh
sudo nomad agent -dev -bind 0.0.0.0 -network-interface eth0
```

Replace `eth0` with your main network interface, shown after `dev` by
`ip route show default`. Leave Nomad running.
In another terminal, `nomad node status` shows one node, `ready`.

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
| `web.nomad.hcl` | A web service under policy `approval`, with `web-smoke` as a post-hook. |
| `web-smoke.nomad.hcl` | The hook: calls the service once it is up. |
| `web.vars.hcl` | The service's image and instance count. |

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

Nops finds that `web` is in git but not in Nomad, and logs
`deployment pending_approval` for it.

## 5. Approve the first deployment

Open <http://localhost:8080> and log in as `admin`, password `change-me`.

The Overview lists `web` as waiting for approval. Open it, check the diff and
click *Approve*.

Nops registers the job, waits for it to be healthy, then runs `web-smoke`.
Within a minute the deployment is **completed**, and `nomad job status web`
shows it running.

## 6. Let a commit deploy on its own

In `nops-jobs`, set the policy to `auto` and ask for two instances:

- in `web.nomad.hcl`: `nops_policy = "auto"`;
- in `web.vars.hcl`: `count = 2`.

```sh
cd nops-jobs
git commit -am "web: deploy on its own, two instances"
cd ..
```

Within a few seconds Nops deploys the commit without asking, then runs the
smoke test again. `nomad job status web` shows two instances running.

## Clean up

Stop Nops with `Ctrl-C`, then:

```sh
nomad job stop -purge web
rm nops.db users
```

Stop Nomad with `Ctrl-C` in its terminal.

## Next

- [Policies](policies.md): pick a policy for each job.
- [Hooks](hooks.md): write your own.
- [Running Nops](running-nops.md): run Nops on a real cluster.
