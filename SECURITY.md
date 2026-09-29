# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Report it
privately through GitHub:
[Report a vulnerability](https://github.com/music-gang/nops/security/advisories/new).

Include what you found, how to reproduce it, and the impact you expect. You
can expect an acknowledgement, and a fix or an explanation, on a best-effort
basis: Nops is maintained by a small team.

## Supported versions

Only the latest release is supported, and while Nops is at `v0` that means the
latest minor: a fix goes into a new patch or minor of the current line, never
into an older one (see
[docs/development.md](docs/development.md#releasing)).

## Scope notes

- The dashboard authenticates the acting user itself, with OpenID Connect or a
  local `username:bcrypt-hash` file depending on `-auth-mode` (see
  [docs/dashboard.md](docs/dashboard.md#authentication)). A reverse proxy in
  front of it is only for TLS.
- Plan diffs are redacted before being stored or shown; a secret that reaches
  the database or the HTML is a valid report.
