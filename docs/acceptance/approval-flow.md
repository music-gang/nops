# Checklist: the approval flow, end to end

Job: [`examples/approval/`](../../examples/approval/). See the
[one-time setup](README.md#one-time-setup) before starting.

## Preconditions

- [ ] nops is running against this repo on the `acceptance-run` branch,
      `-git-path=examples`, logged into the dashboard.
- [ ] `web` is **not** registered in Nomad yet.

## Steps

1. **Create, approve.** Leave `web.vars.hcl` as is. Wait for a poll (or push
   an empty commit). Expect a `pending_approval` deployment for `web` with a
   full-job (create) diff.
   - [ ] `POST /deployments/{id}/approve` (the button) moves it to `pre_hook`
         → `applying` → `post_hook` (the `web-smoke` hook) → `completed`.
   - [ ] `GET /history` lists it as `completed`, with your username in
         "approved by" and the timestamp.
   - [ ] `GET /deployments/{id}` shows the event timeline in order
         (`detected` → `pending_approval` → `pre_hook`/no pre-hook →
         `applying` → `post_hook` → `completed`) and the actor recorded on
         the approval event is your username, not `nops`.
2. **Reject.** Bump `var.count` in `web.vars.hcl` to `2`, commit and push.
   Expect a new `pending_approval` deployment (a scale-up diff). Reject it.
   - [ ] The deployment shows `rejected` in `/history`, with your username.
   - [ ] `nomad job status web` still shows `count = 1`: nothing was applied.
   - [ ] `GET /drift` still shows `web` drifting (the rejected change), since
         the live job never changed.
3. **Supersede.** Bump `var.count` to `2` again (a second commit — same
   target spec as the rejected one is fine, this checks that a *new* commit,
   not a resubmit of the old one, is what creates a fresh deployment) and
   immediately, before approving, push a third commit changing `var.image`
   to a different tag instead.
   - [ ] The first pending deployment (the one from this step) becomes
         `superseded` in `/history`, without ever being decided.
   - [ ] A new `pending_approval` deployment appears reflecting the latest
         commit's diff (image change, possibly combined with the earlier
         count change if it is still uncommitted-over). Approve it and
         confirm `completed` as in step 1.
4. **Approval is spec-bound.** Push a commit that changes `web.vars.hcl`
   (e.g. bump `count` again) while a `pending_approval` deployment for a
   *different* pending change is open but before approving it, then try to
   approve using the dashboard button on the now-stale page (if you have it
   open in two tabs, or reload only after copying the old `spec_hash` from
   the page source).
   - [ ] Approving a `spec_hash` that no longer matches the deployment's
         current one is refused (409, a notice to review the new diff), the
         deployment stays `pending_approval`, and nothing is applied twice.
5. **Drift.** Once `web` is `completed` and stable, change something on the
   live job **outside nops**: `nomad job run` a hand-edited copy with a
   different `count`, bypassing the repo.
   - [ ] The next detection cycle revalidates: either the existing pending
         deployment (if any) is revalidated against the new live index, or,
         with none pending, `GET /drift` shows `web` drifting between the
         live job and the repo's spec.
   - [ ] Push the repo's `web.vars.hcl` back to match what you just set by
         hand (or leave it, confirming instead that nops proposes to bring
         the live job back to the repo's spec, never the other way around —
         invariant "git is the source of truth").

## Cleanup

`nomad job stop -purge web web-smoke`.
