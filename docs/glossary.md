# Glossary

The words Nops uses, in the docs, the dashboard, and the code. If another page
disagrees with this one, this page wins. Code-only terms are in
[development](development.md#terms-used-in-the-code).

## Words shared with Nomad

| Word | In Nops | In Nomad |
|---|---|---|
| **deployment** | One attempt to bring a job to a spec, tracked by Nops. | The rolling update Nomad runs after a register: write **Nomad deployment**. |
| **job** | A Nomad job. Qualify it: **live job**, **target job**, **hook job**, **dispatched job**. | The same. |
| **version** | The Nops build (`v0.1.0`): write **Nops version**. | A job's version: write **job version**. |
| **stop** | Deregister without purge. Nops stops only timed-out hook runs and unused hook revisions. | The same. |

## Jobs and specs

| Term | Meaning |
|---|---|
| **live job** | The job as it is in Nomad now. |
| **spec** | A job's HCL as Nomad parses it. The **target spec** is the one in git. |
| **spec hash** | Identifies what a deployment does: the target spec plus its hooks. An approval is valid for one spec hash. |
| **target job** | The job a deployment is about. |
| **managed namespace** | A namespace in `-nomad-namespaces`. |
| **managed job** | A job with `nops_managed = "true"`. |
| **meta key** | A `nops_*` key in a job's `meta` block ([meta keys](meta-keys.md)). |
| **policy** | `auto`, `approval` or `none`: who gives the go-ahead ([policies](policies.md)). |
| **drift** | A difference between the target spec and the live job. |
| **plan** | Nomad's dry run of a register, with its diff. |
| **diff** | The plan's changes. Nops saves and shows it [redacted](dashboard.md#secret-redaction). |

## Deployment lifecycle

| Term | Meaning |
|---|---|
| **detection** | Comparing git with Nomad and creating, superseding, or revalidating deployments. One pass is a **detection cycle**. |
| **state** | Where a deployment or a hook run is: see [deployment lifecycle](deployment-lifecycle.md). |
| **active deployment** | One not in a terminal state. A job has at most one. |
| **terminal state** | `completed`, `failed`, `rejected` or `superseded`. |
| **approve / reject** | A person's decision on a deployment in `pending_approval`, on the dashboard or through the [API](api.md). |
| **actor** | Who did something: a user name, or `nops` for Nops itself. |
| **API token** | A secret that lets a script act as the person who created it ([API](api.md)). |
| **allowlist** | The users and groups allowed to log in with OIDC. |
| **supersede** | Replace a deployment that hasn't started applying with a newer one. |
| **revalidation** | Re-checking every pending deployment on each detection cycle. |
| **blocked drift** | Drift Nops doesn't redeploy after a failure or a rejection. A **retry** or a new commit unblocks it. |
| **retry** | A person trying a failed or rejected deployment again. The new deployment follows the policy. |
| **hold** | Something that keeps Nops from starting a deployment for a job: a **pause** or a closed **sync window**. |
| **pause / resume** | A person holding a job and releasing it. |
| **sync window** | When an `auto` job may deploy on its own ([policies](policies.md#sync-window)). |
| **deploy now** | A person deploying a job held by its sync window once, without waiting for the window to open. |
| **sync state** | Where a job stands against git, in one word ([dashboard](dashboard.md#sync-state-of-a-job)). |
| **orphan** | A job Nops deployed that is gone from git but still runs in Nomad. Nops shows it and never stops it. |
| **apply** | The register of the target spec, after a fresh plan. |
| **healthy** | What ends an apply: see [architecture](architecture.md#healthy). |
| **promotion wait** | An apply waiting for someone to promote its canaries. The apply timeout pauses meanwhile. |
| **Nomad panel** | The dashboard box with what Nomad reports about a job. |
| **CAS** | Compare-and-set: Nomad registers only if the job's modify index is the one Nops read. |
| **recovery** | Resuming unfinished deployments after a restart. |
| **event** | A row of a deployment's audit log. |
| **notification** | A message to people when a deployment needs them ([notifications](logs-and-notifications.md#notifications)). |
| **notification adapter** | One way to deliver it: webhook, Discord, Slack, ntfy, Gotify. |

## Hooks

| Term | Meaning |
|---|---|
| **hook** | A job Nops runs before (`pre`) or after (`post`) an apply ([hooks](hooks.md)). |
| **phase** | `pre` or `post`. |
| **hook job** | The parameterized batch job in git marked `nops_role = "hook"`. |
| **hook revision** | A hook job at one exact spec, registered in Nomad under a name with its hash. |
| **frozen hook** | A hook as the deployment saw it when created. Approving covers it. |
| **dispatch** | Asking Nomad to start a run of a hook job. |
| **dispatched job** | The job Nomad creates for a dispatch. |
| **dispatch meta** | The `nops_*` values Nops passes to a hook. |
| **hook run** | One run of one hook for one deployment. |
| **position** | A hook's place among the hooks of its phase. |
| **timeout** | How long a hook may run (`nops_timeout`). |
