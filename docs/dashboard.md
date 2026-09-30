# Dashboard

A web dashboard (`net/http` + `html/template`, no JS framework) served by
[`internal/web`](../internal/web). It is organized around what an operator
does with it, not around Nops's tables (see the [decision log](archive/decisions.md),
2026-09-24, "redesigned for what an operator does with it"):

| The operator asks | Where |
|---|---|
| Is anything waiting on me? | **Overview**: what needs attention, what is moving, whether Nops itself works |
| Do I approve this? | **Deployment**: a review page, with what approving will do |
| Is the cluster what git says? | **Jobs**: every managed job, `none` policy included, with its sync state |
| What happened to this job? | **Job**: its drift, hooks, meta issues and every deployment it had |
| What happened, in general? | **Activity**: every deployment, by day |

plus the **git webhook** that triggers an out-of-turn fetch (setting it up:
[running Nops](running-nops.md#the-git-webhook)). How the pages are built is in
[development](development.md#the-dashboards-code).

## Pages

Every page below needs a login (see [authentication](#authentication)); the
header shows the logo, the current tab (Overview, Jobs, Activity), the actor and a
logout button, and the footer the Nops version. Times read relative ("3m ago"), with the absolute UTC time on
hover; long identifiers (a spec hash, an evaluation ID) read short, with the
whole value on hover.

| Page | Shows |
|---|---|
| `GET /` **Overview** | A strip with **Git** (the commit Nops is on: short SHA, linked when the repository is an `http(s)` URL, subject, author, when it was made, when the repository was last checked, and a *Fetch now* button; the last poll's failure, if the latest poll failed) and **Detection** (when the last cycle ran and how long it took, how many managed jobs it found, and `ok`, `degraded` or `aborted`; jobs skipped because Nomad failed on them, files Nomad could not parse, and the error that stopped a cycle, if any; while a file does not parse, a note that jobs removed from git are not being looked for). Then **Needs attention**, most urgent first, one line each with why and where to act: deployments waiting for approval (oldest first, *Review*), blocked jobs (with the reason and a *Retry* button), recent failures nobody retried (7 days), jobs with a meta error, **paused** jobs (who paused, why, and a *Resume* button: listed whether or not the job drifts, so a forgotten pause does not silently stop a job converging), and, last because nothing is broken, **orphans** ("Removed from git, still running in Nomad"). A job under policy `none` that drifts is **not** listed here: leaving it alone is its policy, and it is on Jobs. Then **In progress**: deployments in `detected`, `pre_hook`, `applying` or `post_hook`. Then **Recently completed**: the 5 most recently completed deployments (by when they completed, not when they were created), so one does not simply vanish from In progress with no sign it succeeded — only `completed`; a recent failure is already in Needs attention, and rejected/superseded are not urgent enough for the Overview. Polls itself every 5s (see [Look and technology](development.md#look-and-technology)); the commit's own time reads "committed", the git strip's other time "repository checked", so a fresh commit and a fresh poll of an unchanged head are never confused. |
| `GET /jobs` **Jobs** (`/drift` redirects here) | One row per managed job from `Engine.Observations()`, and one per **orphan** (`Engine.Orphans()`: **Not in git**, no file, the policy of its last deployment): its sync state, policy, last deployment and file. A filter by sync state with counts (`?state=`), kept across the page's own polling. |
| `GET /jobs/{namespace}/{job}` **Job** | The job's sync state, policy, file and hooks (`nops_pre_hook`, `nops_post_hook`: every hook of each phase, in the order they run, with the timeout its hook job sets, none shown for a hook that is not in the repository), the block and its *Retry* when blocked, the job's [sync window](policies.md#sync-windows) in the Details column (whether it is open or closed, until when, the rule, and the time zone it is read in, written in full so it is never read in another), a notice while a closed sync window holds its drift (and when it opens), a notice with *Resume* while it is paused (who, when and why; nothing new is started and Approve is refused, drift is still shown) or, when it is not, a **Pause** box with an optional reason (outside the polled regions, so a reason being typed survives a poll; a job that is not in the repository has none), the [Nomad panel](#the-nomad-panel), the current drift diff with its summary, the meta issues, and its deployments, newest first. A job no longer in the repository still shows its past deployments; one with neither is a 404. An orphan shows a notice instead of a drift: Nops deployed it, it is not in git any more, it still runs in Nomad and Nops does not stop it; the notice says what to do (`nomad job stop -namespace <ns> <id>`, or put the file back) and goes away on its own once either is done. The head, the Nomad panel, the deployments list and the details column poll themselves every 5s; the drift diff does not, so a `<details>` node a person opened or closed stays as they left it. |
| `GET /deployments/{id}` **Deployment** | A header with the job (linked), state, commit (SHA, subject, author), policy and age, and *Open in Nomad* when the Nomad UI address is known; the error, if it failed; a notice when it is what blocks its job (with *Retry*); a notice while its job is paused (with *Resume*; while `pending_approval` its *Approve* is disabled, *Reject* is not); a notice while it waits for a person to promote the canaries of its Nomad deployment (the apply timeout does not run meanwhile), with a *Promote* button and, if the Nomad UI address is known, an *Open in Nomad* link to the job's deployments; while it is `applying`, the [Nomad panel](#the-nomad-panel); while `pending_approval`, the **Review** panel: what Approve will do, in order (the pre-hook, the register, waiting for health, the post-hook: the hooks are the ones the deployment froze, each with the short revision it will run, and the timeouts are the ones of the frozen hook jobs; IDs only, never the specs), and the hook runs listed in the order they run (pre before post, by position), and the Approve and Reject buttons. Then the plan diff with a summary (how many fields are added, edited and removed, and where), open to be reviewed and folded once there is nothing to decide; the hook runs; the timeline (an event that leaves the state as it was, like the wait for a promotion or the request to promote, shows the state's name and its message); and a Details column. Never renders `job_spec` (see [secret redaction](#secret-redaction)). |
| `GET /deployments/{id}/status` | An [htmx](https://htmx.org) fragment, polled by the deployment page every 3s while the deployment is non-terminal, so an approval or a hook finishing elsewhere shows up without a reload: the head and decision panel replace themselves, and the hooks, timeline and details swap out of band. The diff is not sent again. It stops polling itself once the deployment is terminal. |
| `GET /history` **Activity** | Every deployment, in progress and finished, newest first, grouped by day (UTC), with who decided; a filter by state with counts, kept across the page's own polling. The finished ones are the most recent 200. |
| `POST /deployments/{id}/approve` | Calls `Engine.Approve(ctx, id, spec_hash, actor)` with the actor from the session and the `spec_hash` shown on the page (always the whole hash, in a hidden field, whatever the page shows short). A `spec_hash` that no longer matches (`ErrStaleApproval`) re-renders the page with a 409 and a notice to review the new diff, rather than approving the wrong spec. A job that is not in the repository as the last cycle read it (`ErrNotInRepo`; the cases are listed in [architecture](architecture.md#rules-detection-keeps)) gets a 409 too, with a notice that there is nothing to approve until it is back; *Reject* still works. A job that is paused (`ErrPaused`) gets a 409 with a notice to resume it first; *Reject* still works. |
| `POST /deployments/{id}/reject` | Calls `Engine.Reject(ctx, id, actor)`. |
| `POST /deployments/{id}/promote` | Calls `Engine.Promote(ctx, id, actor)`: promotes the canaries of the Nomad deployment an `applying` deployment waits on ([architecture](architecture.md#the-apply-timeout)). It registers nothing. `303` back to the deployment; `409` ("Nothing to promote") when it is not waiting for a promotion any more, as Nomad reports it now (already promoted, or the Nomad deployment moved on); `404` for an unknown deployment or a namespace Nops does not manage; an error from Nomad (for instance a token without `submit-job`) is a `500` with the request on the timeline. |
| `POST /jobs/{namespace}/{job}/retry` | Calls `Engine.Retry(ctx, namespace, job, actor)`: lifts the block of a job whose drift a failed or rejected deployment suppresses, without a new commit. It applies nothing: the next deployment follows the policy (see [deployment lifecycle](deployment-lifecycle.md#not-retrying-an-unchanged-failure)). `303` back to the page named by the form's `back` (`overview`, `jobs`, `activity` or `job`; anything else is the Overview); `409` when the job is no longer blocked or was already retried (a double click, or a newer deployment replaced the failed one); `404` for a namespace Nops does not manage. |
| `POST /jobs/{namespace}/{job}/pause` | Calls `Engine.Pause(ctx, namespace, job, actor, reason)` with the form's optional `reason` (500 bytes at most): holds the job so Nops starts no new deployment for it until it is resumed ([deployment lifecycle](deployment-lifecycle.md#holding-a-job)). It creates and approves nothing. `303` back to the page named by `back` (as for retry); `409` when the job is already paused; `404` when the job is not in the repository as the last cycle read it, or in a namespace Nops does not manage; `400` for a reason that is too long. |
| `POST /jobs/{namespace}/{job}/resume` | Calls `Engine.Resume(ctx, namespace, job, actor)`: lifts the pause; the next detection cycle, asked for at once, treats the job as any other. `303` back like retry; `409` when it is not paused (already resumed since the page was rendered); `404` for a namespace Nops does not manage. |
| `POST /fetch` | Asks the git watcher for a poll now (`Watcher.Trigger`, the same non-blocking trigger as the webhook), instead of waiting for the poll interval. Answers `303` to `/` without waiting for the poll. Served only when the dashboard has a trigger (always, in `cmd/nops`). |
| `GET /healthz` | `200` with `ok <version>` (`ok v0.1.0`), no session needed: what an orchestrator or a load balancer probes. |

### Sync state of a job

Where a managed job stands against git, in one word: the first that applies
wins, and it is also the order of the filter on Jobs. An orphan is not a
managed job any more and has no observation: it only ever has "Not in git".

| State | When |
|---|---|
| **Invalid meta** | one of its `nops_*` keys has an *error* (a warning does not count): Nops ignores its policy and hooks |
| **Paused** | a person paused it (`Observation.Hold`): Nops starts no new deployment for it, drift is still shown. It shows with or without drift, and wins over everything below it, a deployment waiting for approval included |
| **Blocked** | a failed or rejected deployment holds its drift back (`Observation.BlockedBy`) |
| **Not in git** | an **orphan**: deployed by Nops, gone from the repository, still running in Nomad (`Engine.Orphans()`). It has no observation, so it is not judged against the other states |
| **Awaiting approval** | its latest deployment is `pending_approval` |
| **Deploying** | its latest deployment is `detected`, `pre_hook`, `applying` or `post_hook` |
| **Held** | it drifts and its [sync window](policies.md#sync-windows) is closed (`Observation.Hold` of kind `window`): Nops deploys it when the window opens. The row says when. A job with nothing to deploy is **In sync**, whatever its window, and a window is not listed in Needs attention: nobody has anything to do |
| **Drift** | the cluster differs from git and nothing is being done about it (policy `none`, or no deployment yet) |
| **In sync** | the cluster is what git says |

### The Nomad panel

On the Job page, and on the Deployment page while the deployment is `applying`,
a read-only **Nomad** box says what Nomad reports about the job, so that finding
out why a deployment sits in `applying` does not need Nomad's own UI:

- the job's status in Nomad, its type and version;
- for each task group, the count the live job asks for next to the allocations
  of the live version, counted by the status Nomad gives them (running, pending,
  failed, lost), how many its Nomad deployment marked healthy, and how many are
  canaries;
- the job's latest Nomad deployment: its status and description ("Deployment is
  running but requires manual promotion"), and for each group healthy against
  desired, canaries placed against desired, promoted or not, and the progress
  deadline. On a Deployment's page it says whether that Nomad deployment is the
  one tracking this deployment's apply (the same `applied_index`), or another;
- a job Nomad does not have says so, and a Nomad that does not answer says so
  in the box (the failure is also logged at ERROR): the rest of the page renders.

Nothing in it is computed by Nops on top of what Nomad says. It is not a copy of
Nomad's UI: no allocation logs, no exec, no per-task events. The panel of a job
is cached for 4 seconds, errors included, so any number of open tabs cost one
set of Nomad calls (the job, its allocations, its latest deployment) per poll,
and a Nomad that is down is asked once per 4 seconds, not once per tab. Without
a Nomad to read (in tests) the box is not shown. It needs `read-job` in the
namespace, which Nops already has ([token ACL](running-nops.md#the-nomad-token)).

### Links into the Nomad UI

With [`-nomad-ui-url`](configuration.md#nomad) set, the dashboard links to Nomad
in four places, each opening in a new tab:

- **Open in Nomad** in the head of the Job page (for every job it shows, an
  orphan included: it still runs in Nomad) and of the Deployment page, to the
  job: `<nomad-ui-url>/ui/jobs/<job>@<namespace>`;
- **Open in Nomad** in the notice of a deployment that waits for a canary
  promotion, and the Nomad deployment's short ID in the Nomad panel, to the
  job's Deployments tab, `<nomad-ui-url>/ui/jobs/<job>@<namespace>/deployments`,
  where the canaries are and where they can be promoted too. Nomad's UI has no
  page of its own for one deployment, so the tab is as close as a link gets.

Unset: none of them, and nothing else changes. The address is never taken from
`-nomad-addr`, which is what Nops itself uses and often not reachable from a
browser. A job ID with a `/` (a hook's dispatched child) is escaped as one
segment, as the UI reads it. The UI is a JavaScript app that answers every
`/ui/` path with the same page, so a request cannot tell a good link from a
wrong one: the routes are checked in Nomad's own bundle by
`TestNomadUIServesTheLinkedRoutes`, against the Nomad version CI tests with, and
not in a browser.

The one write here is the *Promote* button of a deployment that waits for its
canaries to be promoted, on the Deployment page only: the Job page's box shows
the wait and the deployment that carries the button.

### Writes and redirects

`POST /jobs/.../retry`, `.../pause` and `.../resume`, `POST /deployments/.../promote` and `POST /fetch` go
through `Auth.Require` like approve and reject, so a cross-origin request is refused before anything is
called. No form value reaches a `Location` header as a path or URL: retry, pause and resume go
back to a page chosen **by name** from a fixed list (`back=jobs`), `/fetch`
always to `/` (see the [decision log](archive/decisions.md), 2026-09-24).

The dashboard never calls `Store.Transition` for a decision or picks the next
state itself: approve and reject always go through the engine (invariant 3).
The handlers work from small interfaces over `*store.Store`, `*engine.Engine`,
`*gitwatch.Watcher` and `*nomadx.Client` (`web.Store`, `web.Engine`, `web.Git`,
`web.Nomad`), so tests use fakes instead of a real database, Nomad client or
repository.

## Authentication

Nops logs people in itself, never a header set by a reverse proxy
(`Remote-User`): Nops cannot tell who set that header — anything that can
reach its port without going through the proxy (another allocation, the LAN,
the tailnet) could approve a deployment by sending it, and a mistake in the
network setup would fail silently. `-auth-mode` picks which of two backends
verifies identity itself instead, so this stays true either way; the two are
mutually exclusive (Nops refuses to start if both are configured) and the
options for each are in [configuration](configuration.md#dashboard):

- **`oidc`**: against the OpenID Connect provider you already run
  (Authentik, Authelia, Keycloak, …). Nops manages no users or passwords of
  its own.
- **`basic`**: local users, an operator-maintained file of usernames and
  bcrypt password hashes. No provider to run — the choice for a small,
  private cluster (or a single machine) with no IdP already in place.

**Every page needs a login**, reads included: the diff of a deployment says
what runs on the cluster. Only these are open: `/auth/*` (the login itself),
the [git webhook](running-nops.md#the-git-webhook) (its own secret), `/healthz` and `/static/*`
(the stylesheet and htmx, embedded in the binary: nothing in them is secret).

After a login, either backend sends the browser back to the page it asked for
(`?next=`), but only if it is a path on the dashboard itself: anything with a
scheme or host, a `//` or `/\` start, a backslash or a control character
(a tab, a newline: browsers drop them before parsing a URL, so `/<TAB>/host`
would be read as `//host`) sends it to `/` instead.

## OpenID Connect (`-auth-mode=oidc`)

### The flow

- A request without a session is sent to `/auth/login` (a `GET`; anything else
  answers 401), remembering the page it wanted. Nops redirects to the provider
  with a random `state`, a `nonce` and PKCE (`S256`), kept in a short-lived,
  signed and encrypted cookie.
- `/auth/callback` checks the `state`, exchanges the code, verifies the ID
  token (signature, issuer, audience, expiry, `nonce`) and reads the user's
  claims. What the allowlist needs and the ID token lacks is read from the
  userinfo endpoint (Authelia keeps most claims out of the ID token), and a
  userinfo answer about another subject is an error.
- The user must be on the **allowlist**: `-oidc-allowed-users` matches the
  `preferred_username` or the `email` (case-insensitive; an email the provider
  marks `email_verified: false` does not count), `-oidc-allowed-groups`
  matches a value of the `groups` claim. **With both empty Nops does not
  start**: every user of the provider could otherwise approve a deployment.
  Someone who authenticates but is not on the list gets 403 and a WARN in the
  log.
- Then Nops sets a **session**: a signed, encrypted cookie (`HttpOnly`,
  `SameSite=Lax`, `Secure` when the public URL is `https`) valid 12 hours.
  The key is random and lives only in the process: **a restart logs everybody
  out** (one redirect when the provider still has its own session).
  `POST /auth/logout` deletes the cookie; it does not end the session at the
  provider.
- The allowlist is checked at login only. Removing someone takes effect when
  their session ends (12 hours at the latest, or a restart of Nops).

### The actor

The user's name is the **actor**: the `preferred_username`, else the `email`,
else the `sub`. It ends up in `decided_by` and in `events.actor`, and it is
the only thing the pages take from the session: approve and reject call
`Engine.Approve`/`Reject` with it.

### An unreachable provider

Nops does not contact the provider at startup, so an outage at the provider
never stops the engine: `auto` deployments keep going. Discovery is tried at
every login, which answers 503 (and logs an ERROR) while it fails, and works
again as soon as the provider does. People already logged in are not affected
until their session ends.

## Local users (`-auth-mode=basic`)

No provider, no claims: `-users-file` holds one `username:bcrypt-hash` line
per user (blank lines and `#` comments ignored), read once at startup — a
bad path, a malformed line or a hash that does not parse as bcrypt is a
startup error naming the line. Generate a line the same way you would for
an `nginx`/Apache basic-auth file:

```sh
htpasswd -nB alice
```

(`-n` prints instead of writing a file, `-B` picks bcrypt; without
`apache2-utils` installed, any bcrypt one-liner in your language of choice
works too — Nops only needs a standard bcrypt hash.) Paste the `user:hash`
line it prints into the file Nops reads.

- **The flow:** `GET /auth/login` shows a plain username/password form,
  `POST /auth/login` checks it. An unknown username still runs a bcrypt
  compare (against a fixed dummy hash), so a wrong username and a wrong
  password look and take the same time to reject — the response is the same
  generic "Invalid username or password", and a WARN is logged either way.
- **The actor** is the username as is: no claims to map, unlike OIDC.
- **Changing a password or removing a user** means editing the file and
  restarting Nops (a running session is not revoked early, same as OIDC's
  allowlist).
- **Known limitation:** there is no lockout or rate limiting on repeated
  failed logins. Acceptable for the personal, self-hosted use this mode
  targets; put Nops behind a reverse proxy that rate-limits if the dashboard
  is reachable from anywhere less trusted than that.

## Secret redaction

`deployments.job_spec` (the full parsed job Nops will register at apply time)
is a **separate column, never redacted**: apply needs the exact spec that was
planned, and it can carry the same secrets as the diff. The dashboard must
never render it (see
[architecture](architecture.md#what-a-cycle-keeps-and-where)).
Only `plan_diff` is meant to be shown.

The diff is redacted **before** being saved to SQLite and rendered in HTML, by
[`internal/redact`](../internal/redact/redact.go) (keep the two aligned). A
secret value becomes `<redacted>`; an empty value stays empty. The field name
and the change type (added, deleted, edited) stay, so the reviewer sees
*which* secret changes and how, never its value.

What is redacted, by the name Nomad gives the diff field (`TestRedactRealPlan`
runs the rules on a real plan):

| What | Field names |
|---|---|
| Every environment variable, whatever its name | `Env[...]` |
| The body of a template | `EmbeddedTmpl` |
| Artifact headers | `GetterHeaders[...]` |
| Service check headers | every field below a `Header` object |
| Any field or object whose name contains `password`, `token`, `secret`, `auth`, `credential`, `privatekey` or `apikey` (case-insensitive, ignoring `-` and `_`) | e.g. `Meta[api_token]`, `X-Api-Key`, the docker `auth[0][password]`; a matching object redacts everything below it |
| The `user:password@` part of a URL, in any value | `https://<redacted>@host/...` |

Everything else is shown as it is: the rules are a denylist. Known limits:

- A secret inside a value with a harmless name is not seen, e.g.
  `args = ["--db-password=..."]`. Put secrets in `env` or in a `template`
  (better: from Nomad Variables or Vault, so the spec holds no secret at all).
- A harmless name can match, e.g. `Meta[author]` contains `auth`. Redacting
  too much is the accepted side of the trade.

See [philosophy](philosophy.md#patterns-reused-from-nomad-gitops).
