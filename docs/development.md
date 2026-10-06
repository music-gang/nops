# Development and testing

How to change Nops: tests, docs, the flow of a change, releases, and Go
conventions.

## What needs which tests

| Area | Tests |
|---|---|
| `internal/meta` | Table-driven unit tests: valid and invalid values, unknown keys, defaults. |
| `internal/engine` | Unit tests for every transition, for crash recovery, and for CAS conflicts. They use a fake Nomad and an injected clock. Each invariant has an adversarial test in `invariants_test.go`. |
| `internal/nomadx` | Unit tests against an `httptest` stub, plus integration tests. |
| `internal/store` | Tests against real SQLite in `t.TempDir()`. |
| `internal/hooks` | Unit tests with a fake Nomad and real SQLite, plus integration tests. |
| `internal/redact` | Unit tests on hand-built and real plan diffs: no secret reaches the database or the HTML. |
| `internal/metrics` | Every metric against fakes. |
| `internal/notify` | `httptest` receivers for every adapter and every failure. |
| `internal/gitwatch` | Tests against a local bare repository. |
| `internal/web` | `httptest` for every page and state, and the login against a fake OIDC provider. |
| `internal/cli` | `httptest` stubs of the API for every command, plus an integration test against the real server. |
| `cmd/nops` | Covered by the integration smoke test. |
| The docs | `go test ./...` checks the option and meta key tables against the code, and the test names and links in the pages. |
| `scripts/release.sh` | `scripts/release_test.sh` and `shellcheck`. |

A behaviour that depends on Nomad itself gets an integration test.

## Integration

Integration tests live in `tests/integration/` behind the `integration` build
tag. They run against `nomad agent -dev` with ACLs, using
`NOPS_TEST_NOMAD_ADDR` and `NOPS_TEST_NOMAD_TOKEN`; without the address they
skip. The end-to-end tests (`e2e_*_test.go`) build the real binary and run it
with [the documented Nomad token](running-nops.md#the-nomad-token), so a
missing capability fails CI. Hooks use `raw_exec` jobs, so the agent must run
on the same host as the tests.

### Nomad version

CI tests against `NOMAD_VERSION` in `.github/workflows/ci.yml`; the README
states the same version, and a test fails if they differ. To move to a new
Nomad, change `NOMAD_VERSION` and `NOMAD_SHA256`, update the README, and run
the integration tests.

## Coverage

At least 80% on `engine`, `store`, `meta` and `hooks`. No target on `web` and
`cmd`.

## Writing docs

The docs follow common open source practice:

- **Structure:** [Diátaxis](https://diataxis.fr). Each page is a tutorial, a
  how-to guide, reference or explanation, and holds only that kind of content.
  The [index](README.md) groups pages the same way.
- **Style:** the [Google developer documentation style guide](https://developers.google.com/style).
  Vale checks it in CI ([commands](#commands)).
- **Content:** what a reader needs to use or change Nops. Leave out what the
  code already answers. State each fact on one page and link to it from the
  others.
- **Commits, issues and pull requests** follow
  [Conventional Commits](#commit-messages) and the repository's templates,
  and say what changes and why.

## Doc audit

Before a release, check the claims of the pages changed since the last tag
against the code and its tests. Report each claim that doesn't hold as *doc
wrong*, *code wrong?*, *untested*, or *unclear*. The maintainer decides the
fixes: doc fixes in one pull request, each behaviour fix in its own with a
failing test first.

## When a piece of work is "done"

1. `go build`, `go vet`, `staticcheck`, `go test -race` and Vale pass.
2. Changes to `engine`, `hooks` or `nomadx` pass the integration tests.
3. Every behaviour change has a test.
4. The docs and `examples/` match the change.

## What to update for each change

| Change | Update |
|---|---|
| Meta key | `internal/meta` and `meta-keys.md` |
| Flag or environment variable | `internal/config` and `configuration.md` |
| Policy behaviour | `policies.md` |
| Hook contract | `hooks.md` and `examples/` |
| State, transition, schema, recovery | `deployment-lifecycle.md` |
| Detection or apply | `architecture.md` |
| Dashboard page or login | `dashboard.md` |
| Logs or notifications | `logs-and-notifications.md` |
| Metric | `metrics.md` |
| Running on a cluster | `running-nops.md` |
| First steps | `getting-started.md` |
| Term | `glossary.md` |
| Invariant | `philosophy.md` |
| Doc page | `nav` in `mkdocs.yml` |
| Build or release | [Releasing](#releasing) |
| Nomad version | `ci.yml` and the README |

## Commands

```sh
go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest -tags integration ./...
vale sync && vale docs README.md CONTRIBUTING.md

# the docs site, previewed on http://127.0.0.1:8000
pip install -r scripts/mkdocs/requirements.txt
mkdocs serve

# integration: a dev agent with ACLs, in another terminal
printf 'acl {\n  enabled = true\n}\n' > agent.hcl
nomad agent -dev -config=agent.hcl &
export NOPS_TEST_NOMAD_ADDR=http://127.0.0.1:4646
export NOPS_TEST_NOMAD_TOKEN=$(curl -s -X POST $NOPS_TEST_NOMAD_ADDR/v1/acl/bootstrap | jq -r .SecretID)
go test -tags integration -race -count=1 ./tests/integration/...
```

## Workflow and CI

1. Pick an open issue with no branch or pull request. Settle open design
   questions in the issue first.
2. Branch from `main` as `type/short-description`.
3. Make the change with its tests and docs.
4. Open a pull request titled `type(scope): subject`. Fill in the template with
   *What changes*, *Why*, and *Notes* for open questions, and add `Closes #N`.
5. After review, push new commits. Don't amend or force-push.
6. The maintainer squash-merges and writes the final commit message.

`main` requires a pull request with these checks green:

| Check | Runs |
|---|---|
| `test` | `gofmt`, `go mod tidy`, build, `go vet`, `go test -race` |
| `lint` | `staticcheck`, `shellcheck`, the script tests, `goreleaser check`, Vale, the docs site build |
| `integration` | The integration tests against a Nomad dev agent |
| `pr-title` | The pull request title is a Conventional Commit |

`govulncheck`, CodeQL, secret scanning, and Dependabot also run. Actions are
pinned to full commit SHAs.

Issues use the forms in `.github/ISSUE_TEMPLATE/`: a plain title, the type
(`Bug`, `Feature` or `Task`), and a label for each area it touches.

## Releasing

A release is a signed `vX.Y.Z` tag on `main`. Before `v1`, a minor release
may break things and a patch never does.

1. Make sure `main` is green and the [doc audit](#doc-audit) is done.
2. For a minor or major release, move the image tag in
   [running-nops.md](running-nops.md) to the new `X.Y` in a pull request
   first: the script refuses to tag without it.
3. Run `scripts/release.sh`. It lists the commits since the last tag,
   suggests a version, then signs and pushes the tag. `--dry-run` only shows
   the suggestion.
4. Ship risky changes as a release candidate first (`v0.2.0-rc.1`).
5. Check the result:
   `docker run --rm ghcr.io/music-gang/nops:0.2.0 -version`.
6. Never move or delete a tag: fix forward with a patch release.

The `release` workflow builds the binary and the image with goreleaser,
publishes them to the GitHub release and `ghcr.io/music-gang/nops`, and
writes the release notes from the commit subjects, with *Upgrade notes* on
top, made from the `BREAKING CHANGE:` footers. A final release also
publishes the docs site. Image tags for `vX.Y.Z`
are `X.Y.Z`, `X.Y` and `latest`. The image is distroless with the binary
only. The version comes from the tag, set at build time with
`-ldflags -X github.com/music-gang/nops/internal/version.version=<tag>`.

Bump by hand: the goreleaser version in `release.yml` and `ci.yml`, and the
Debian release of the base image.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/) in the Angular
style:

```
<type>(<scope>): <subject>

<body: what changed and why>

<footer: BREAKING CHANGE, issue references, Co-Authored-By>
```

- **Types:** `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
  `build`, `ci`, `chore`, `revert`.
- **Subject:** imperative, lowercase, no trailing period, 72 characters at
  most. It becomes a line of the release notes, so write it for someone
  running Nops.
- **Body:** wrapped at 72 characters, plain text. Say what changed and why.
- **Breaking change:** `!` after the scope and a `BREAKING CHANGE:` footer
  that says what changed and what to do. It becomes an item of the
  release's *Upgrade notes*.

## The dashboard's code

Pages are `html/template` with plain CSS and [htmx](https://htmx.org), vendored
under `/static`. Every action works as a plain form without JavaScript. The
look follows [GitHub's Primer](https://primer.style): light and dark themes,
one item per line. Deployment states use one color each: pending amber,
running blue, completed green, failed red, and rejected or superseded grey.
Static files are linked with a content hash, so a browser never mixes old and
new versions.

Requests that change state refuse cross-origin requests
(`http.CrossOriginProtection`) on top of `SameSite=Lax` cookies.

## Terms used in the code

The words of [the glossary](glossary.md), plus:

| Term | Meaning |
|---|---|
| **Nops** / **`nops`** | **Nops** is the product; **`nops`** in code font is the binary, image, module, or a name in code. |
| **store** | `internal/store`. |
| **engine** | `internal/engine`. Its loops are **detection** (also called the **reconciler**) and **apply**. |
| **runner** | `hooks.Runner`: runs one hook run to the end. |
| **sentinel error** | An exported `Err...` value checked with `errors.Is`. |
| **fake / stub** | A fake is an in-memory stand-in; a stub is an `httptest` server with canned answers. |
| **in flight** | An issue with a branch or an open pull request. |
| **decision log** | [`archive/decisions.md`](archive/decisions.md): decisions up to v0.4.0, no longer updated. |

## Go conventions

- English everywhere: code, comments, docs, commits.
- `gofmt`, `go vet` and `staticcheck`.
- Short, singular package names. No `util` or `common`.
- `context.Context` first in every function that does I/O.
- Every state change goes through `Store.Transition`.
- Wrap errors with context: `fmt.Errorf("dispatch hook %s: %w", id, err)`.
  Export sentinels only when callers need them.
- Consumers define small interfaces.
- No `init()` with side effects, no global state; inject time.
- Log with `log/slog`.
- Every flag has its `NOPS_*` variable; every interval and timeout is
  configurable.
- Prefer an established package to writing a new library.
