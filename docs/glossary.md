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
| **stop** | Deregister without purge. | The same. |

## Jobs and specs

| Term | Meaning |
|---|---|
| **live job** | The job as it is in Nomad now. |
| **spec** | A job's HCL as Nomad parses it. The **target spec** is the one in git. |
| **spec hash** | Identifies what a deployment does: the target spec plus its hooks. |
| **target job** | The job a deployment is about. |
| **managed namespace** | A namespace in `-nomad-namespaces`. |
| **managed job** | A job with `nops_managed = "true"`. |
| **policy** | `auto`, `approval` or `none`: who gives the go-ahead ([policies](policies.md)). |
| **drift** | A difference between the target spec and the live job. |

## Deployment lifecycle

| Term | Meaning |
|---|---|
| **detection** | Comparing git with Nomad and creating, superseding, or revalidating deployments. One pass is a **detection cycle**. |
| **active deployment** | One not in a terminal state. |
| **terminal state** | `completed`, `failed`, `rejected` or `superseded` ([deployment lifecycle](deployment-lifecycle.md)). |
| **actor** | Who did something: a user name, a token's name, or `nops` for Nops itself. |
| **supersede** | Replace a deployment that hasn't started applying with a newer one. |
| **revalidation** | Re-checking every pending deployment on each detection cycle. |
| **blocked drift** | Drift Nops doesn't redeploy after a failure or a rejection. |
| **hold** | Something that keeps Nops from starting a deployment for a job: a **pause** or a closed **sync window**. |
| **sync window** | When an `auto` job may deploy on its own ([policies](policies.md#sync-window)). |
| **deploy now** | A person deploying a job held by its sync window once, without waiting for the window to open. |
| **sync state** | Where a job stands against git, in one word ([dashboard](dashboard.md#sync-state-of-a-job)). |
| **orphan** | A job Nops deployed that is gone from git but still runs in Nomad ([architecture](architecture.md#orphan-jobs)). |
| **apply** | The register of the target spec, after a fresh plan. |
| **promotion wait** | An apply waiting for someone to promote its canaries. |
| **CAS** | Compare-and-set: Nomad registers only if the job's modify index is the one Nops read. |
| **event** | A row of a deployment's audit log. |

## Access control

See [access control](acl.md).

| Term | Meaning |
|---|---|
| **token** | What a request to Nops carries: a public **accessor ID** and a secret. |
| **management token** | A token that can do everything. |
| **client token** | A token that can do what its ACL policies allow. |
| **bootstrap token** | The management token in `-acl-bootstrap-token-file`. |
| **ACL policy** | A named set of rules that grants capabilities. Never shortened to **policy**, which is a job's. |
| **binding rule** | What a login gets: it matches the login's claims and binds an ACL policy or management. |
| **session token** | The token a login creates. |
| **capability** | One thing an ACL policy allows, such as `approve`. |

## Hooks

See [hooks](hooks.md).

| Term | Meaning |
|---|---|
| **hook** | A job Nops runs before (`pre`) or after (`post`) an apply. |
| **hook job** | The parameterized batch job in git marked `nops_role = "hook"`. |
| **hook revision** | A hook job at one exact spec, registered in Nomad under a name with its hash. |
| **frozen hook** | A hook as the deployment saw it when created. |
| **dispatched job** | The job Nomad creates for a run of a hook job. |
| **dispatch meta** | The `nops_*` values Nops passes to a hook. |
| **hook run** | One run of one hook for one deployment. |
