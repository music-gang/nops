# Checklist: pre-pull on a host volume

Job: [`examples/prepull-hostvolume/`](../../examples/prepull-hostvolume/).
Read [hooks.md](../hooks.md#pre-pull-on-a-host-volume) first for why this
pattern exists. See the [one-time setup](README.md#one-time-setup) before
starting.

## Preconditions

- [ ] The `myapp_data` host volume exists on one node and is declared in that
      node's `client` stanza (see the comment at the top of
      `myapp.nomad.hcl`).
- [ ] nops is running against this repo on the `acceptance-run` branch,
      `-git-path=examples`, logged into the dashboard.
- [ ] `myapp` is **not** registered in Nomad yet (`nomad job status myapp` →
      not found), so the first deployment is a plain create.

## Steps

1. **First deploy.** Leave `myapp.vars.hcl` as is. Wait for a poll (or push
   an empty commit to force one). Expect: a `pending_approval` deployment for
   `myapp` on the dashboard, with a full-job diff (create). Approve it.
   - [ ] `myapp-prepull` is registered and dispatched (`pre_hook` on the
         deployment page), then the deployment moves to `applying`.
   - [ ] `nomad job status myapp` shows a running allocation once `completed`.
2. **Time the pull.** `docker image rm` the current `myapp` image tag on the
   volume's node, so the next deploy pulls cold. Note the image's size.
3. **Bump the image.** Pick a different tag of a sizeable image (or the same
   `nginx` tag after step 2) in `myapp.vars.hcl`, commit and push to
   `acceptance-run` (see [triggering a deployment](README.md#triggering-a-deployment)).
   - [ ] A new `pending_approval` deployment appears for `myapp` with an
         image-only diff. Approve it.
   - [ ] The deployment page shows `myapp-prepull` running in `pre_hook`
         **before** `applying` starts; `docker images` on the node shows the
         new tag pulled while the hook runs, before the old `myapp`
         allocation is stopped.
   - [ ] Once the hook succeeds, `applying` starts and finishes quickly (the
         image is already cached): time from `applying` to `completed` is
         close to a plain container restart, not a full pull.
4. **Timeout path (optional but recommended once).** Set
   `nops_pre_hook_timeout` in `myapp.nomad.hcl` to something the pull cannot
   beat (e.g. `"1s"`) for one run, bump the image again, push.
   - [ ] The deployment goes to `failed`, `myapp-prepull`'s dispatched job is
         stopped (not purged: still visible in `nomad job status
         myapp-prepull`), and `myapp` itself was never touched
         (`nomad job status myapp` still shows the previous version).
   - [ ] Revert the timeout before the next run.

## Cleanup

`nomad job stop -purge myapp myapp-prepull`; remove the volume's data if you
want a clean slate for the next run.
