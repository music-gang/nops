# Changelog

What changes in each release of Nops, newest first. Before upgrading, read
[Upgrading](docs/running-nops.md#upgrading). Release candidates are on the
[GitHub releases](https://github.com/music-gang/nops/releases) only.

## v0.13.0

### Breaking changes

- `GET /api/deployments` and `GET /api/jobs` answer a page of 50 rows, with a
  `next_token` while more follow. A client that reads every row follows
  `next_token` until it's absent ([lists](docs/api.md#lists),
  [#222](https://github.com/music-gang/nops/pull/222)).

### Features

- `web`: page, search, and filter the lists of deployments and jobs. Activity,
  Jobs and a job's deployments show a page at a time, with a search box and
  filters by state, policy, and repository. The API and `nops deployments` and
  `nops jobs` take the same parameters ([API lists](docs/api.md#lists),
  [command-line lists](docs/cli.md#lists),
  [#222](https://github.com/music-gang/nops/pull/222)).

## v0.12.2

### Other changes

- Built with Go 1.27.3, with the dependencies updated
  ([#219](https://github.com/music-gang/nops/pull/219)).
- The image's base, `distroless/static-debian13`, moves to a newer digest
  ([#215](https://github.com/music-gang/nops/pull/215)).

## v0.12.1

### Fixes

- `engine`: register the next version with the file from git as its source
  ([#214](https://github.com/music-gang/nops/pull/214))

## v0.12.0

### Features

- `engine`: pass the commit to the job as the `nops_commit` variable
  ([#210](https://github.com/music-gang/nops/pull/210))
- limit the jobs a repository may declare
  ([#209](https://github.com/music-gang/nops/pull/209))

### Fixes

- `web`: check the next path before the redirect to the login
  ([#206](https://github.com/music-gang/nops/pull/206))

## v0.11.0

### Features

- `web`: link ACL policies from the token pages to a read-only view
  ([#198](https://github.com/music-gang/nops/pull/198))
- `web`: list the repositories on their own page
  ([#204](https://github.com/music-gang/nops/pull/204))
- read jobs from several git repositories
  ([#202](https://github.com/music-gang/nops/pull/202))

### Fixes

- `engine`: stop failing a health wait on a scale
  ([#199](https://github.com/music-gang/nops/pull/199))

## v0.10.1

### Fixes

- `engine`: stop reading a scale as an outside edit
  ([#195](https://github.com/music-gang/nops/pull/195))
- `nomadx`: stop deploying a new job with a scaling block twice
  ([#194](https://github.com/music-gang/nops/pull/194))

## v0.10.0

### Breaking changes

- `-metrics-token-file`, `NOPS_METRICS_TOKEN_FILE` and `NOPS_METRICS_TOKEN`
  are removed, and Nops refuses to start with them. With the ACL on, give
  Prometheus a token whose ACL policy has `capabilities = ["metrics"]`; with it
  off, `/metrics` is open
  ([#184](https://github.com/music-gang/nops/pull/184)).

### Features

- `metrics`: protect `/metrics` with the ACL instead of its own token
  ([#184](https://github.com/music-gang/nops/pull/184))
- `cli`: complete commands and flags in `bash` and `zsh`
  ([#186](https://github.com/music-gang/nops/pull/186))
- `metrics`: expose when each token expires
  ([#182](https://github.com/music-gang/nops/pull/182))
- mark the token in use in the list of tokens
  ([#176](https://github.com/music-gang/nops/pull/176))

### Fixes

- `engine`: fail a deployment whose job is removed from Nomad while applying
  ([#180](https://github.com/music-gang/nops/pull/180))

## v0.9.0

### Features

- `cli`: log in from the command line with `nops login`
  ([#163](https://github.com/music-gang/nops/pull/163))
- pause every job at once
  ([#172](https://github.com/music-gang/nops/pull/172))
- take a backup of the database on request
  ([#173](https://github.com/music-gang/nops/pull/173))

### Fixes

- `store`: refuse a database newer than the schema
  ([#171](https://github.com/music-gang/nops/pull/171))

## v0.8.0

### Breaking changes

- The upgrade deletes the existing API tokens: create new ones on the
  Administration page or with `nops acl token create`. Nops refuses to start
  with `-oidc-allowed-users` or `-oidc-allowed-groups`: limit who can log in at
  the identity provider, or use binding rules with `-acl`.

### Features

- add access control with ACL policies, tokens, and binding rules
- `cli`: generate a Nops secret from the command line
  ([#145](https://github.com/music-gang/nops/pull/145))

### Fixes

- `deps`: build with Go 1.27.2 and `golang.org/x/net` v0.60.0
  ([#162](https://github.com/music-gang/nops/pull/162))
- `release`: start a final release's notes from the last final release
  ([#160](https://github.com/music-gang/nops/pull/160))
- `web`: show error pages on boosted forms and links
  ([#158](https://github.com/music-gang/nops/pull/158))

## v0.7.0

### Breaking changes

- The server runs as `nops serve`: put `serve` before its flags
  (`nops -git-url ...` becomes `nops serve -git-url ...`). The image runs it
  with no arguments; a Nomad job that passes flags in `args` must start them
  with `serve` ([#126](https://github.com/music-gang/nops/pull/126)).

### Features

- `cli`: add a command-line client and run the server as `nops serve`
  ([#126](https://github.com/music-gang/nops/pull/126))
- `notify`: say who asked for a Deploy now in a notification
  ([#121](https://github.com/music-gang/nops/pull/121))
- `web`: add a JSON API with per-user tokens
  ([#124](https://github.com/music-gang/nops/pull/124))
- `web`: serve an OpenAPI description of the API
  ([#133](https://github.com/music-gang/nops/pull/133))

### Fixes

- `web`: send an htmx request without a session to the login
  ([#123](https://github.com/music-gang/nops/pull/123))

## v0.6.0

### Features

- `engine`: deploy a held job outside its sync window
  ([#116](https://github.com/music-gang/nops/issues/116),
  [#120](https://github.com/music-gang/nops/pull/120))
- `notify`: say what changed, who and where in a notification
  ([#114](https://github.com/music-gang/nops/pull/114))

## v0.5.0

### Features

- `engine`: retry a failed deployment from its page
  ([#111](https://github.com/music-gang/nops/pull/111))
- `metrics`: expose Prometheus metrics at `/metrics`
  ([#107](https://github.com/music-gang/nops/pull/107))

### Fixes

- `engine`: show a promotion from the dashboard at once
  ([#108](https://github.com/music-gang/nops/pull/108))
- `hooks`: fail a refused hook dispatch with Nomad's answer
  ([#110](https://github.com/music-gang/nops/pull/110))
- `nomadx`: parse a job in a managed namespace
  ([#103](https://github.com/music-gang/nops/pull/103))

## v0.4.0

### Features

- `engine`: pause a job from the dashboard
  ([#88](https://github.com/music-gang/nops/pull/88))
- `engine`: sync windows for automatic deployments
  ([#89](https://github.com/music-gang/nops/pull/89))
- `web`: link jobs and deployments to the Nomad UI
  ([#85](https://github.com/music-gang/nops/pull/85))
- `web`: show a job's Nomad health and promote canaries
  ([#84](https://github.com/music-gang/nops/pull/84))

### Fixes

- `engine`: keep a held job's block visible
  ([#90](https://github.com/music-gang/nops/pull/90))
- `engine`: wait for a canary promotion without failing the deployment
  ([#83](https://github.com/music-gang/nops/pull/83))

## v0.3.0

### Features

- `web`: add the logo and the icons of the browser tab and the home screen
  ([#68](https://github.com/music-gang/nops/pull/68))

## v0.2.2

### Fixes

- `engine`: create a deployment in the state it waits in
  ([#61](https://github.com/music-gang/nops/pull/61))
- `engine`: log `deployment_id` once on an apply transition
  ([#64](https://github.com/music-gang/nops/pull/64))
- `engine`: refuse to approve a job that is not in git
  ([#62](https://github.com/music-gang/nops/pull/62))

## v0.2.1

### Fixes

- `engine`: a file that fails to parse is not read as a removal
  ([#53](https://github.com/music-gang/nops/pull/53))
- `engine`: a target that never has an allocation is healthy once registered
  ([#57](https://github.com/music-gang/nops/pull/57))
- `engine`: do not remember a failed parse
  ([#54](https://github.com/music-gang/nops/pull/54))
- `engine`: log a failed deployment at ERROR
  ([#55](https://github.com/music-gang/nops/pull/55))
- `engine`: never advance a detected deployment under approval
  ([#52](https://github.com/music-gang/nops/pull/52))

## v0.2.0

### Features

- `notify`: opt-in notification when a deployment completes
  ([#49](https://github.com/music-gang/nops/pull/49))
- `release`: interactive script to cut a release
  ([#50](https://github.com/music-gang/nops/pull/50))
- `web`: serve the dashboard under a sub path
  ([#47](https://github.com/music-gang/nops/pull/47))
- `web`: show recently completed deployments on the Overview
  ([#48](https://github.com/music-gang/nops/pull/48))

## v0.1.0

### Features

- `config`: add flags and environment variables with validation
  ([#8](https://github.com/music-gang/nops/pull/8))
- `config`: default `-public-url` to the local listen address
  ([#23](https://github.com/music-gang/nops/pull/23))
- `config`, `notify`: notification adapters and plain environment variable
  secrets ([#10](https://github.com/music-gang/nops/pull/10))
- `engine`: apply deployments from approval to completed
  ([#14](https://github.com/music-gang/nops/pull/14))
- `engine`: detect drift and create deployments
  ([#13](https://github.com/music-gang/nops/pull/13))
- `engine`: manage several Nomad namespaces from one instance
  ([#40](https://github.com/music-gang/nops/pull/40))
- `engine`: resume interrupted deployments after a restart
  ([#16](https://github.com/music-gang/nops/pull/16))
- `engine`, `store`, `gitwatch`, `web`: data and actions for the redesigned
  dashboard ([#27](https://github.com/music-gang/nops/pull/27))
- `engine`, `store`, `nomadx`, `web`: freeze hooks with the deployment and
  register them as revisions ([#33](https://github.com/music-gang/nops/pull/33))
- `engine`, `store`, `web`: report jobs removed from git that still run in Nomad
  ([#32](https://github.com/music-gang/nops/pull/32))
- `gitwatch`: add the in-memory git watcher
  ([#12](https://github.com/music-gang/nops/pull/12))
- `hooks`: add hook runner with dispatch, wait, and timeout
  ([#3](https://github.com/music-gang/nops/pull/3))
- `meta`, `store`, `hooks`, `engine`, `web`: run several hooks per phase, with
  the timeout on the hook ([#34](https://github.com/music-gang/nops/pull/34))
- `nomadx`: add Nomad client wrapper with CAS-only register
- `redact`: redact secrets from the plan diff
  ([#9](https://github.com/music-gang/nops/pull/9))
- `release`: publish a versioned image and binary on a `vX.Y.Z` tag
  ([#42](https://github.com/music-gang/nops/pull/42))
- `web`: add OIDC login for the dashboard
  ([#17](https://github.com/music-gang/nops/pull/17))
- `web`: dashboard pages and git webhook
  ([#18](https://github.com/music-gang/nops/pull/18))
- `web`: local users as a second login backend
  ([#19](https://github.com/music-gang/nops/pull/19))
- `web`: poll Overview, Jobs, Job, and Activity for live updates
  ([#36](https://github.com/music-gang/nops/pull/36))
- `web`: redesign the dashboard around what an operator does with it
  ([#28](https://github.com/music-gang/nops/pull/28))
- `wiring`: `cmd/nops` entrypoint
  ([#20](https://github.com/music-gang/nops/pull/20))

### Fixes

- `engine`: ask for a detection cycle when a deployment ends
  ([#35](https://github.com/music-gang/nops/pull/35))
- `engine`: fail a deployment whose register keeps failing past the apply
  timeout ([#41](https://github.com/music-gang/nops/pull/41))
- `web`: refuse control characters in the post-login redirect
  ([#26](https://github.com/music-gang/nops/pull/26))
- `web`: version the static files so a changed style sheet is downloaded
  ([#30](https://github.com/music-gang/nops/pull/30))
