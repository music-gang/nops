# Acceptance checklists

These are manual checklists for scenarios that need a real Nomad cluster:
heavy images, host volumes, real storage. `tests/integration/` (a
`nomad agent -dev`, see [development.md](../development.md#integration))
already covers everything that does not.

- [`prepull-hostvolume.md`](prepull-hostvolume.md): pre-pulling a heavy image
  on a host-volume node before a stop+start deploy.
- [`backup-stateful.md`](backup-stateful.md): a fresh backup before a
  stateful deploy.
- [`approval-flow.md`](approval-flow.md): approve, reject, supersede, and an
  outside edit (drift), end to end.

The jobs used by each checklist live in [`examples/`](../../examples/), one
directory per scenario, and are runnable as they are (each has sensible
defaults) — no fixture repository to set up, no jobs to write by hand.

## One-time setup

1. A Nomad cluster (or a single node is enough) with the `docker` driver, and
   the host volumes each checklist's job comment lists, declared in that
   node's `client` stanza and created on disk.
2. A branch of this repository to point nops at, so a run never touches
   `main`:

   ```sh
   git checkout -b acceptance-run
   git push -u origin acceptance-run
   ```

3. A local users file for `-auth-mode=basic` (see
   [dashboard.md](../dashboard.md#local-users)):

   ```sh
   htpasswd -nB acceptance > /tmp/nops-users
   ```

4. Run nops against this repository, `examples/` as `-git-path`, the branch
   above, and a scratch database:

   ```sh
   nops \
     -git-url=https://github.com/music-gang/nops \
     -git-branch=acceptance-run \
     -git-path=examples \
     -auth-mode=basic -users-file=/tmp/nops-users \
     -db-path=/tmp/nops-acceptance.db \
     -public-url=http://127.0.0.1:8080 \
     -nomad-addr=http://<your-cluster-address>:4646
   ```

   Add `-nomad-token-file`/`-nomad-tls-skip-verify`/etc. as your cluster
   needs. Open `http://127.0.0.1:8080` and log in with the user from step 3.

## Triggering a deployment

Each checklist's job has a `variable` (image tag, count, …) meant to be
edited for the run. Change it in the job's `.vars.hcl` file, on the
`acceptance-run` branch, and push:

```sh
git add examples/<scenario>/<name>.vars.hcl
git commit -m "chore(acceptance): bump <name>"
git push
```

nops picks it up on the next poll (`-git-poll-interval`, default `1m`), or
immediately if the `-webhook-secret-file` is set and the forge is configured
to call `/webhook/git`.

## Recording a run

A checklist is "run" when every step passed against a real cluster. Note,
next to the checklist (a comment in the PR that turns `acceptance` to `done`,
or a line added under this section) the date, the nops commit, and the Nomad
version used. `docs/roadmap.md`'s "Done when" for `acceptance` is met once
every checklist below has one such note.

## Cleanup

`nomad job stop -purge` the jobs, remove the host volumes' data, delete
`acceptance-run` and the scratch database when done.
