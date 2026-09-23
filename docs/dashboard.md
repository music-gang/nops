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

The diff is redacted **before** being saved to SQLite and rendered in HTML
(`Env[...]`, templates, keys containing password/token/secret). See
[philosophy](philosophy.md#patterns-reused-from-nomad-gitops).
