# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Report it
privately through GitHub:
[Report a vulnerability](https://github.com/music-gang/nops/security/advisories/new).

Include what you found, how to reproduce it, and the impact you expect. You
can expect an acknowledgement, and a fix or an explanation, on a best-effort
basis: nops is maintained by a small team.

## Supported versions

nops is in early development and has no releases yet. Only the latest commit on
`main` is supported.

## Scope notes

- The dashboard reads the acting user from a header set by a reverse proxy (see
  [docs/dashboard.md](docs/dashboard.md)). Exposing nops directly, without the
  proxy, is a misconfiguration, not a vulnerability.
- Plan diffs are redacted before being stored or shown; a secret that reaches
  the database or the HTML is a valid report.
