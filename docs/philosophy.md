# Philosophy and invariants

Nops is a GitOps controller for a self-hosted Nomad cluster for personal use.
It stays small: a feature that only exists to scale doesn't get built.

## Invariants

No change may break these rules. The tests that try to break them are in
`internal/engine/invariants_test.go`.

1. **Plan before every write.** Nops registers a job only after a fresh plan
   shows a difference. *Why:* no useless job versions, and the plan's diff is
   what the dashboard shows.

2. **CAS on every register.** Nops registers with the job's modify index read
   at detection, so Nomad refuses the write if anyone changed the job since.
   *Why:* time passes between detection and apply, especially while a
   deployment waits for approval. Nops never overwrites a manual change.

3. **Never auto-apply under policy `approval`.** Only a logged-in person moves
   a deployment past `pending_approval`, and the approval is valid only for
   the spec they saw. *Why:* that's the point of the policy.

4. **Git is the source of truth for Nops's behaviour.** Policy and hooks come
   from the HCL in git, never from the live job. An invalid meta key means
   policy `none` and an ERROR. *Why:* you change what Nops does with a
   reviewed commit. A person's pause is the one exception, and it can only
   make Nops do less.

5. **Nops never writes to Git** and never writes meta into the live job; its
   state lives only in SQLite. *Why:* meta written into a live job vanishes at
   the next `nomad job run`.

6. **One active deployment per job**, enforced by a unique index in the
   database. *Why:* an index survives a crash; a mutex doesn't.

7. **State is persisted before acting.** Nops saves what it's about to do
   before it touches Nomad, and stops if the write fails. *Why:* after a
   crash, Nops knows where it was, and resumes safely.

## Fail loud

Nops stops and says so rather than carry on with uncertain state:

- It never ignores a Nomad or SQLite error. A failed notification is the only
  soft failure: a WARN.
- When in doubt, it takes the safe path: invalid meta means policy `none`, a
  missing hook fails the deployment, a CAS conflict fails it too.
- Every error tells you what failed and where.

## Patterns reused from nomad-gitops

Nops reuses these ideas from
[gerrowadat/nomad-gitops](https://github.com/gerrowadat/nomad-gitops): Nomad
parses the HCL, the plan's diff drives both the decision and the display,
registers use CAS, the newest commit supersedes older ones, secrets are
redacted from the diff, and groups with a scaling policy keep their live count.

It leaves out the stateless design, since approvals and hooks need state, and
it never stops your jobs.
