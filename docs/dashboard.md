# Dashboard

> To be completed once `internal/web` exists. What follows are the decisions
> already taken.

A web dashboard (`net/http` + `html/template`, no JS framework) with:

- a list of **pending deployments**, PR-style: the plan diff (with secrets
  redacted) and approve/reject buttons;
- a **history** of past deployments, with who approved and when (from
  `decided_by`, `decided_at` and `events`).

## Authentication

nops logs people in itself with **OpenID Connect**, against the provider you
already run (Authentik, Authelia, Keycloak, …). It manages no users or
passwords. The options are in [configuration](configuration.md#dashboard).

Why not a header set by a reverse proxy (`Remote-User`)? nops cannot tell who
set it: anything that can reach its port without going through the proxy (
another allocation, the LAN, the tailnet) could approve a deployment by
sending the header, and a mistake in the network setup would fail silently.
With OIDC the identity is a signed token that nops verifies. A reverse proxy
in front for TLS is still fine; it just is not part of the trust.

**Every page needs a login**, reads included: the diff of a deployment says
what runs on the cluster. Only these are open: `/auth/*` (the login itself),
the git webhook (its own secret) and the health check.

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

### Writes and CSRF

A request that changes state (`POST`) is refused with 403 when the browser says
it comes from another origin (`Sec-Fetch-Site`, or `Origin` against `Host`:
Go's `http.CrossOriginProtection`), on top of the `SameSite=Lax` cookie. There
is no CSRF token to carry through the pages. Requests with neither header (a
`curl`) are allowed, and still need the session cookie.

### An unreachable provider

nops does not contact the provider at startup, so an outage at the provider
never stops the engine: `auto` deployments keep going. Discovery is tried at
every login, which answers 503 (and logs an ERROR) while it fails, and works
again as soon as the provider does. People already logged in are not affected
until their session ends.

### Setting up the client

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
