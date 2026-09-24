# Checklist: backup before a stateful deploy

Job: [`examples/backup-stateful/`](../../examples/backup-stateful/). See the
[one-time setup](README.md#one-time-setup) before starting.

## Preconditions

- [ ] The `db_data` and `db_backup` host volumes exist on the same node and
      are declared in that node's `client` stanza (see the comment at the
      top of `db.nomad.hcl`).
- [ ] nops is running against this repo on the `acceptance-run` branch,
      `-git-path=examples`, logged into the dashboard.
- [ ] `db` is **not** registered in Nomad yet.

## Steps

1. **First deploy.** Leave `db.vars.hcl` as is. Wait for a poll (or push an
   empty commit). Approve the `pending_approval` deployment for `db` (a plain
   create: `db-backup` still runs as the pre-hook, but there is nothing to
   dump yet — expect it to fail or produce an empty dump against a database
   that does not exist; that is fine for a first create and does not block
   apply from the same acceptance run's point of view, but note it so it is
   not mistaken for a real failure).
   - [ ] `nomad job status db` shows a running allocation once `completed`.
2. **Seed data.** Connect to the running database
   (`nomad alloc exec <alloc> -- psql -U postgres -c "..."`, or `psql` from
   outside if the port is reachable) and create something you can check for
   after a restore: a table with a row, a marker value.
3. **Trigger a real deploy.** Bump `var.image` in `db.vars.hcl` (a different
   Postgres patch tag is enough, e.g. `postgres:16.4-alpine`), commit and
   push to `acceptance-run`.
   - [ ] A new `pending_approval` deployment appears with an image-only diff.
         Approve it.
   - [ ] The deployment page shows `db-backup` running in `pre_hook`
         **before** `applying` starts.
   - [ ] Once `db-backup` succeeds, a dump file named after the deployment ID
         exists under the `db_backup` volume's path on disk
         (`<deployment_id>.dump`).
   - [ ] `applying` starts only after the backup succeeded, and the
         deployment reaches `completed`.
4. **Prove the backup is usable.** Restore the dump into a scratch database
   (`pg_restore` from the same `postgres:16-alpine` image, against a
   throwaway database) and confirm the marker from step 2 is present.
5. **Failure path (optional but recommended once).** Make the backup fail on
   purpose for one run — e.g. temporarily rename `db_backup`'s mount point on
   the node so the write fails, or stop the `db` service before pushing a
   bump — then bump the image and push.
   - [ ] The deployment goes to `failed`, and `db`'s live job is untouched
         (still the previous image): `nomad job status db` confirms it.
   - [ ] Undo the induced failure before the next run.

## Cleanup

`nomad job stop -purge db db-backup`; remove both volumes' data if you want a
clean slate for the next run.
