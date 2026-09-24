# Dashboard

A web dashboard (`net/http` + `html/template`, no JS framework) served by
[`internal/web`](../internal/web), with:

- a list of **pending deployments**, PR-style: the plan diff (with secrets
  redacted) and approve/reject buttons;
- a **history** of past deployments, with who approved and when (from
  `decided_by`, `decided_at` and `events`);
- the **drift** of every managed job, `none` policy included, since those
  never get a deployment;
- one **page per deployment**, with the diff, the decision, the hook runs and
  the event timeline;
- the **git webhook** that triggers an out-of-turn fetch.

## Pages

Every page below needs a login (see [authentication](#authentication)); the
header shows the current tab, the actor and a logout button.

| Page | Shows |
|---|---|
| `GET /` | Deployments in state `pending_approval` (a count up top: the one thing the page wants you to notice), then every other active deployment (`detected`, `pre_hook`, `applying`, `post_hook`). |
| `GET /history` | Terminal deployments (`completed`, `failed`, `rejected`, `superseded`), newest first, with who decided and when. |
| `GET /drift` | `Engine.Observations()`: one row per managed job, including `none` policy, with its drift, its meta validation issues and, when a retry rule is suppressing a new deployment, `BlockedBy`/`BlockedReason`. |
| `GET /deployments/{id}` | The plan diff, the decision rail (state, commit, spec hash, Approve/Reject when `pending_approval`), the hook runs and the event timeline. Never renders `job_spec` (see [secret redaction](#secret-redaction)). |
| `GET /deployments/{id}/status` | An [htmx](https://htmx.org) fragment: the decision rail without the diff, polled by the deployment page every 3s while the deployment is non-terminal, so an approval or a hook finishing elsewhere shows up without a reload. It stops polling itself once the deployment is terminal. |
| `POST /deployments/{id}/approve` | Calls `Engine.Approve(ctx, id, spec_hash, actor)` with the actor from the session and the `spec_hash` shown on the page. A `spec_hash` that no longer matches (`ErrStaleApproval`) re-renders the page with a 409 and a notice to review the new diff, rather than approving the wrong spec. |
| `POST /deployments/{id}/reject` | Calls `Engine.Reject(ctx, id, actor)`. |
| `POST /jobs/{namespace}/{job}/retry` | Calls `Engine.Retry(ctx, namespace, job, actor)`: lifts the block of a job whose drift a failed or rejected deployment suppresses, without a new commit. It applies nothing: the next deployment follows the policy (see [state-machine](state-machine.md#not-retrying-an-unchanged-failure)). `303` back to the optional `next` form value (confined to this site, like the post-login redirect) or `/`; `409` when the job is no longer blocked or was already retried (a double click, or a newer deployment replaced the failed one); `404` for another namespace. |
| `POST /fetch` | Asks the git watcher for a poll now (`Watcher.Trigger`, the same non-blocking trigger as the webhook), instead of waiting for the poll interval. Answers `303` back to `next` or `/` without waiting for the poll. Served only when the dashboard has a trigger (always, in `cmd/nops`). |
| `GET /healthz` | `200 ok`, no session needed: what an orchestrator or a load balancer probes. |

Both writes go through `Auth.Require` like approve and reject, so a
cross-origin request is refused before anything is called. No page has the
buttons yet: they arrive with the redesigned pages
([roadmap](roadmap.md#dashboard-ux)).

The dashboard never calls `Store.Transition` for a decision or picks the next
state itself: approve and reject always go through the engine (invariant 3).
Both handlers work from a small interface over `*store.Store` and
`*engine.Engine` (`web.Store`, `web.Engine`), so tests use fakes instead of a
real database or Nomad client.

## Look and technology

No JS framework, no build step: `html/template` renders every page (all
templates parsed once at startup, so a broken one fails loud rather than on
the first request), plain CSS carries the design, and
[htmx](https://htmx.org) is the only script, vendored under `/static` rather
than loaded from a CDN. Every action works as a plain form post without it;
htmx adds `hx-boost` (page navigation without a full reload) and the status
polling above. The CSP is `script-src 'self'; style-src 'self'` — there is no
inline script or style to allow.

The visual design reads
[Airbnb's](https://github.com/VoltAgent/awesome-design-md/blob/main/design-md/airbnb/DESIGN.md)
(white/ink canvas, soft rounded corners, one accent used sparingly, a single
loud typographic moment) with nops's own accent (a deep teal, not Airbnb's
pink-red, which would read as "danger" next to the failed/reject states), a
dark mode Airbnb itself does not have, and Inter/JetBrains Mono in place of
Airbnb Cereal. See the [decision log](design/decisions.md) (2026-09-24).
Deployment states map to one of five colors used consistently across every
page: pending (amber), running — `pre_hook`/`applying`/`post_hook` — (blue),
completed (green), failed (red), rejected/superseded (muted grey).

The plan diff renders Nomad's `JobDiff` recursively (job → task groups →
tasks → objects/fields) as nested `<details>`, open only where something
changed; a redacted value (`<redacted>`) renders as a pill rather than plain
text, so it reads as "a secret changed here" at a glance.

## Authentication

nops logs people in itself, never a header set by a reverse proxy
(`Remote-User`): nops cannot tell who set that header — anything that can
reach its port without going through the proxy (another allocation, the LAN,
the tailnet) could approve a deployment by sending it, and a mistake in the
network setup would fail silently. `-auth-mode` picks which of two backends
verifies identity itself instead, so this stays true either way; the two are
mutually exclusive (nops refuses to start if both are configured) and the
options for each are in [configuration](configuration.md#dashboard):

- **`oidc`**: against the OpenID Connect provider you already run
  (Authentik, Authelia, Keycloak, …). nops manages no users or passwords of
  its own.
- **`basic`**: local users, an operator-maintained file of usernames and
  bcrypt password hashes. No provider to run — the choice for a small,
  private cluster (or a single machine) with no IdP already in place.

**Every page needs a login**, reads included: the diff of a deployment says
what runs on the cluster. Only these are open: `/auth/*` (the login itself),
the [git webhook](#git-webhook) (its own secret) and `/healthz`.

After a login, either backend sends the browser back to the page it asked for
(`?next=`), but only if it is a path on the dashboard itself: anything with a
scheme or host, a `//` or `/\` start, a backslash or a control character
(a tab, a newline: browsers drop them before parsing a URL, so `/<TAB>/host`
would be read as `//host`) sends it to `/` instead.

## OpenID Connect (`-auth-mode=oidc`)

### The flow

- A request without a session is sent to `/auth/login` (a `GET`; anything else
  answers 401), remembering the page it wanted. nops redirects to the provider
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
  matches a value of the `groups` claim. **With both empty nops does not
  start**: every user of the provider could otherwise approve a deployment.
  Someone who authenticates but is not on the list gets 403 and a WARN in the
  log.
- Then nops sets a **session**: a signed, encrypted cookie (`HttpOnly`,
  `SameSite=Lax`, `Secure` when the public URL is `https`) valid 12 hours.
  The key is random and lives only in the process: **a restart logs everybody
  out** (one redirect when the provider still has its own session).
  `POST /auth/logout` deletes the cookie; it does not end the session at the
  provider.
- The allowlist is checked at login only. Removing someone takes effect when
  their session ends (12 hours at the latest, or a restart of nops).

### The actor

The user's name is the **actor**: the `preferred_username`, else the `email`,
else the `sub`. It ends up in `decided_by` and in `events.actor`, and it is
the only thing the pages take from the session: approve and reject call
`Engine.Approve`/`Reject` with it.

### An unreachable provider

nops does not contact the provider at startup, so an outage at the provider
never stops the engine: `auto` deployments keep going. Discovery is tried at
every login, which answers 503 (and logs an ERROR) while it fails, and works
again as soon as the provider does. People already logged in are not affected
until their session ends.

### Setting up the client

`-public-url` is no longer required to start nops (see
[configuration](configuration.md#dashboard)): unset, it defaults to a
`localhost` guess derived from `-listen-addr`, which is essentially never
right for OIDC — a provider redirects the browser to the registered URI, not
to wherever nops happens to be listening. **Set `-public-url` explicitly**
before configuring the client below.

Create an OIDC client (confidential, authorization code) at the provider:

- **Redirect URI:** `<public-url>/auth/callback`, e.g.
  `https://nops.example.com/auth/callback`.
- **Scopes:** `openid`, `profile`, `email`, and `groups` when you use
  `-oidc-allowed-groups` (nops asks for `groups` only then). Check that the
  provider puts a `groups` claim in the ID token or in userinfo: with Authelia
  that is the `groups` scope, with Authentik the group mapping of the client.
- **Issuer:** the value in the provider's discovery document
  (`<issuer>/.well-known/openid-configuration`), trailing slash included:
  Authentik's is like `https://auth.example.com/application/o/nops/`.

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
works too — nops only needs a standard bcrypt hash.) Paste the `user:hash`
line it prints into the file nops reads.

- **The flow:** `GET /auth/login` shows a plain username/password form,
  `POST /auth/login` checks it. An unknown username still runs a bcrypt
  compare (against a fixed dummy hash), so a wrong username and a wrong
  password look and take the same time to reject — the response is the same
  generic "Invalid username or password", and a WARN is logged either way.
- **The actor** is the username as is: no claims to map, unlike OIDC.
- **Changing a password or removing a user** means editing the file and
  restarting nops (a running session is not revoked early, same as OIDC's
  allowlist).
- **Known limitation:** there is no lockout or rate limiting on repeated
  failed logins. Acceptable for the personal, self-hosted use this mode
  targets; put nops behind a reverse proxy that rate-limits if the dashboard
  is reachable from anywhere less trusted than that.

## Writes and CSRF

A request that changes state (`POST`) is refused with 403 when the browser says
it comes from another origin (`Sec-Fetch-Site`, or `Origin` against `Host`:
Go's `http.CrossOriginProtection`), on top of the `SameSite=Lax` cookie, for
both login backends alike — this includes `POST /auth/login` itself,
against session-fixation-style login CSRF, not only the dashboard's own
writes. There is no CSRF token to carry through the pages. Requests with
neither header (a `curl`) are allowed, and still need the session cookie
where one is required.

## Git webhook

`POST /webhook/git` asks the git watcher for an out-of-turn poll
(`Watcher.Trigger()`, non-blocking: it does not wait for the poll to finish)
so a push shows up sooner than `-git-poll-interval`. It needs no session — it
is authenticated by a secret shared with the forge instead
(`-webhook-secret-file`, [configuration](configuration.md#dashboard)) — and
is disabled (404) when that secret is unset. nops never looks at the payload
beyond checking its signature: any push is "something changed, go look", so
there is nothing forge-specific to parse.

Each forge signs differently, and nops checks whichever header is present:

| Forge | Header | How it is checked |
|---|---|---|
| GitHub | `X-Hub-Signature-256: sha256=<hex>` | HMAC-SHA256 of the raw body with the secret, compared with `hmac.Equal`. |
| Gitea | `X-Gitea-Signature: <hex>` | Same construction, no `sha256=` prefix. |
| GitLab | `X-Gitlab-Token: <secret>` | The header must equal the secret itself, compared in constant time. |

A request matching none of them, or whose signature does not match, gets 401
and a WARN in the log that never says which check failed (so a probe cannot
learn which forge nops expects). The body is capped at 1 MiB.

Set up the webhook at the forge: URL `<public-url>/webhook/git`, content
type `application/json`, secret the same file's content passed to
`-webhook-secret-file` (or `NOPS_WEBHOOK_SECRET`). Only a push to
`-git-branch` needs to trigger it, but nops does not filter by branch or ref:
any authenticated request just triggers a poll, which is a no-op if nothing
under `-git-path` changed.

## Secret redaction

`deployments.job_spec` (the full parsed job nops will register at apply time)
is a **separate column, never redacted**: apply needs the exact spec that was
planned, and it can carry the same secrets as the diff. The dashboard must
never render it (see
[engine-detection](design/engine-detection.md#job_spec-keeps-the-full-unredacted-spec)).
Only `plan_diff` is meant to be shown.

The diff is redacted **before** being saved to SQLite and rendered in HTML, by
[`internal/redact`](../internal/redact/redact.go) (keep the two aligned). A
secret value becomes `<redacted>`; an empty value stays empty. The field name
and the change type (added, deleted, edited) stay, so the reviewer sees
*which* secret changes and how, never its value.

What is redacted, by the name Nomad gives the diff field (verified on Nomad
2.0.3):

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
