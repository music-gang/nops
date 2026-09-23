# Dashboard

> To be completed once `internal/web` exists. What follows are the decisions
> already taken.

A web dashboard (`net/http` + `html/template`, no JS framework) with:

- a list of **pending deployments**, PR-style: the plan diff (with secrets
  redacted) and approve/reject buttons;
- a **history** of past deployments, with who approved and when (from
  `decided_by`, `decided_at` and `events`).

## Authentication

nops sits behind a reverse proxy (Authelia, oauth2-proxy, Tailscale serve…)
and reads the user from a configurable header (`NOPS_AUTH_HEADER`, default
`Remote-User`).

- Write actions (approve/reject) **without the header answer 403**.
- The user ends up in `decided_by` and in `events.actor`.
- Write actions are `POST` only, with a CSRF token.
- nops manages no users or passwords: it trusts the header, so it **must not
  be exposed directly**, only behind the proxy.

## Secret redaction

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
