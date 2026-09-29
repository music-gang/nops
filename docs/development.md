# Development and testing

## What needs which tests

| Area | Kind of test |
|---|---|
| `internal/meta` | Table-driven unit tests: valid values, invalid values, unknown keys, defaults. |
| `internal/engine` | Table-driven unit tests for **every** transition, including crash recovery and CAS conflicts, with an in-memory fake Nomad and an injected clock. |
| `internal/nomadx` | Unit tests against an `httptest` stub of the Nomad API (request shape, error mapping) + integration tests against a real Nomad. |
| `internal/store` | Against **real SQLite** on a temporary file (`t.TempDir()`), not mocks: constraints, the lock index, migrations. |
| `internal/hooks` | Unit tests with an in-memory fake Nomad, a **real SQLite** store and an injected clock (success, failure, timeout, resume, redispatch with token, Nomad/SQLite errors) + integration with `raw_exec` hooks. |
| `internal/redact` | Unit: no secret must reach the DB or the HTML, over hand-built diffs and a fixture from a real plan (`testdata/plan_diff.json`) + integration: the same rules on a live plan. |
| `internal/notify` | `httptest` receivers: the request of every adapter, several adapters at once, and non-2xx, timeout, unreachable and cancelled (a WARN each, no error, no URL in the log). |
| `internal/gitwatch` | Unit tests against a local bare repository over `file://` (new commit, no change, force-push, coalesced triggers, vars pairing, `-git-path` scoping, a failed fetch keeps the snapshot). Skipped if `git` is not on `PATH`. |
| `internal/web` | `httptest` for handlers, with fakes for the store, the engine and git and a fixed clock: every page in every state it has (empty, pending, blocked, failed, invalid meta, a store error), the helpers (relative times, the diff summary, sync state, the plan steps), that the whole `spec_hash` is in the approve form and `job_spec` never on a page; the login against a fake OIDC provider (`httptest` serving discovery, keys, token and userinfo, signing real ID tokens), cross-origin refusals and 401/403. No coverage target. |
| `cmd/nops` | Almost entirely straight-line wiring, so almost entirely covered by the integration smoke test below, not unit tests: a unit test only for the one piece of actual logic (`newAuthenticator` picking the login backend by `-auth-mode`). No coverage target. |
| The docs | Tests inside `go test ./...` that read the pages: the option tables of `configuration.md` and the key table of `meta-keys.md` against `internal/config` and `internal/meta` (both directions, defaults included), and `docs/docs_test.go`, which fails when a test named in any `.md` (a name ending in `*` is a group) is not a function in the repo. They prove the page and the code agree on names and defaults, not that a cited test asserts the claim. |
| `scripts/release.sh` | `scripts/release_test.sh` (plain bash, run by the `lint` job) covers the pure functions: the suggested bump, the next version, the release candidate number, semver validation and order. `shellcheck` on every script. The flow around them (git, `gh`, prompts) is looked at with `--dry-run`, never in CI: it would tag. |
| Real interaction with Nomad | Integration. |

## Integration

- Build tag `integration`, in `tests/integration/`. They run against a local
  `nomad agent -dev`. If `NOPS_TEST_NOMAD_ADDR` is not set they are skipped
  (skip, not fail).
- Hooks are tested with **lightweight jobs**, never with heavy images:
  - `raw_exec` (enabled in dev mode): `true` → success, `false` → failure,
    `sleep 600` → timeout;
  - docker driver, when available: `busybox`/`alpine`, to check the passing of
    `nops_image_<task>` and placement via host volume.
- Realistic scenarios (pre-pulling a heavy image, a backup before a stateful
  deploy) are **simulated**, not run on a real cluster: the same guarantees
  (the pre-hook runs before the register and gets the deployment ID, a failing
  or slow hook leaves the live job alone, a failure is not retried in a loop)
  are checked with `raw_exec` hooks that append to a log or write a marker
  file. What a simulation cannot say (how long a real pull takes, whether a
  real dump restores, the cluster's own ACLs and TLS) is judged by using nops
  on the cluster.
- The **end-to-end tests** (`e2e_*_test.go`) build the real `nops` binary once
  and run it as a subprocess against a scratch git repository (a bare repo
  over `file://`, pushed to by the test) and the Nomad under test: basic
  auth, 200ms intervals, the dashboard driven over HTTP and nops's own SQLite
  read alongside it. `TestE2EApprovalFlow` covers create, approve, reject,
  superseding, a stale `spec_hash` (409) and an outside edit under policy
  `approval`; `TestE2EAutoRevertsOutsideEdit` the same edit under `auto`; the
  `TestE2EPreHook*` tests the hook scenarios above; `TestE2ESeveralHooksRunInOrderAroundTheApply` and `TestE2EAFailingFirstPreHookKeepsTheSecondFromRunning` several hooks per phase (in order, and a failure stopping the phase); `TestE2EHookRevisionLifecycle` and `TestE2EChangedHookMustBeApprovedAgain` the hook revisions (not in Nomad before approval, registered and dispatched after it, deregistered once unused, a changed hook approved again); `TestE2EOrphan*` a deployed job removed from git (shown, never stopped, gone
  once stopped or restored); `TestE2ERetry*` the retry of
  a blocked job (a failed pre-hook fixed without a commit, and under `approval`
  the retry still waiting for a decision), `TestE2EFetchNow` the "fetch now"
  button with a one-hour poll interval and `TestE2EOneInstanceManagesSeveralNamespaces`
  one instance over two namespaces created on the agent (the same job ID in
  both, one `auto` with a hook and one `approval`, and a job in the unlisted
  `default` refused). The hooks write to a
  test directory, so they need the Nomad agent on the same host as the tests
  (true for `nomad agent -dev`).
- `TestNopsBinaryStartsAndShutsDown` is the smoke test of the same harness:
  `/healthz` answers, then `SIGTERM` and a clean exit within the shutdown
  grace period.
- `TestExamplesParse` parses every job and hook in `examples/` with Nomad's
  own parser, checks its `nops_*` meta, that each declared hook exists and
  that nothing needs Consul (every service has `provider = "nomad"`, no file
  mentions Consul). It does not run the examples: their docker images are
  not pulled in CI.
- `TestMain` drops the `NOMAD_*` variables of the environment, so a shell set
  up for a real cluster is never used by a test.
- A dev agent answers `429` past 100 connections from one address, so a test
  that creates a Nomad client closes its idle connections when it ends
  (`newClient`).

### First rollout on a real cluster

The real check is using it. The policies allow going in steps: start with
`nops_policy = "none"` on a few jobs and watch `/drift`; move stateful jobs to
`approval` and stateless ones to `auto` one at a time.

## Coverage

≥ 80% on `engine`, `store`, `meta`, `hooks` (`go test -cover`). No target on
`web` and `cmd`.

## Doc audit

The tests above catch names and defaults that drift. What they cannot catch is a
claim that was never true, or stopped being: an invariant, a transition, what a
page says a test covers. A **doc audit** checks those, and is done before a
release (`scripts/release.sh` reminds you) or when someone asks for it. The code
and its tests are the evidence; the docs, their cross-references and the
decision log are only the claims being checked.

**Scope.** The pages and packages changed since the last tag:
`git diff --stat <last-tag>..HEAD -- '*.md' cmd internal scripts examples .github`.
The first audit, or one with no tag, covers everything.

**What it does:**

1. **Inventory the claims** of the pages in scope, plus the pages that describe
   a package in scope: invariants and state transitions, defaults, flag and env
   names, meta keys, errors and notifications, what the dashboard shows, what a
   test is said to cover, and the decision-log rows still in force.
2. **Check each against the code**, noting where it is implemented and which
   test proves it, with one verdict: *OK* (code and a test agree), *UNTESTED*
   (the code seems to do it, no test proves it), *DOC WRONG* (the code is
   intentional, the doc is stale), *CODE WRONG?* (the doc is the intent, the
   code does something else) or *UNCLEAR*. Skip what the guards already check.
3. **Claims that depend on Nomad's own behaviour** (what bumps
   `JobModifyIndex`, what a parse accepts) are settled by an integration test
   against `nomad agent -dev`, not by reading: write it, run it, and keep it.
4. **Report** the non-OK findings grouped by verdict (claim, doc location, code
   location, proposed fix), the number of OK claims per page, and for every
   *CODE WRONG?* and *UNCLEAR* the options and a recommendation. The maintainer
   decides; nothing changes before.
5. **Fix** as agreed: the doc-only fixes and the missing tests in one PR, every
   behaviour change in its own, each with a test that fails before the fix and a
   decision-log row.

A finding that is a fact stated on several pages is fixed by stating it once and
linking (as the notifications and *healthy* are), not by editing each copy.
The first audit (decision log, 2026-09-28) found about 30 of these in roughly
560 claims.

## When a piece of work is "done"

A commit/PR is complete only if:

1. `go build ./...`, `go vet ./...`, `staticcheck` and `go test -race ./...` are green;
2. if it touches `engine`, `hooks` or `nomadx`, the integration tests are also
   green against a `nomad agent -dev`;
3. every behaviour change has its test;
4. the docs (and `examples/`) are updated if the HCL syntax, states or a
   decision change.

## Commands

```sh
go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest -tags integration ./...

nomad agent -dev &                     # in another terminal
NOPS_TEST_NOMAD_ADDR=http://127.0.0.1:4646 go test -tags integration -race -count=1 ./tests/integration/...
```

`staticcheck`: with a recent Go, the binary in `~/go/bin` may have been built
with an older Go and fail with "export data version"; `go run ...@latest`
avoids the problem.

## Workflow and CI

`main` is protected by a repository ruleset: a pull request is required, the
checks below must pass on an up-to-date branch, history must be linear, and
force pushes and deletion are blocked. No approvals are required (there is a
single maintainer): CI is the gate. Release tags have their own two rulesets,
described in [Releasing](#releasing).

The step-by-step flow — branching, committing, opening a PR, the PR
description's `## What changes` / `## Why` / `## Notes` shape — is in
[CLAUDE.md](../CLAUDE.md#picking-up-work). What's specific to this repo's CI
and merge mechanics, not covered there:

- Commits inside a branch do not need to be signed, whoever makes them: the
  squash commit on `main` is created and signed by GitHub, and the ruleset
  does not require signed commits. Locally the maintainer commits with his
  GPG key; a commit made anywhere else is unsigned.
- Merges are squash-only, so `main` is a straight line with one commit per
  PR. The repository's squash setting is "Default to pull request title and
  commit details" (`squash_merge_commit_message = COMMIT_MESSAGES`), which
  prefills the squash box from the PR title and the branch's own commit
  messages — but the maintainer edits that box by hand before merging, since
  he is the one who merges. The branch is deleted automatically.

The PR template (`.github/pull_request_template.md`) has these headings:

```markdown
## What changes

- concrete, present-tense bullet points

## Why

- the motivation, one bullet per reason

## Notes
```

It is only the three headings (`## Notes` is optional, see CLAUDE.md: delete
it when there is nothing to say), and
every section is filled in by hand, never left as a placeholder (see the
decision log, 2026-09-24: an earlier template leaked its HTML comments and
an unfilled checklist into commit bodies, `a933a4c` and `8052989`, back when
the squash setting copied the PR description verbatim). The template
applies to a PR opened from the GitHub web page; `gh pr create --body-file`
replaces it. The "done" checklist stays in
[When a piece of work is "done"](#when-a-piece-of-work-is-done), not in the
PR body. If the branch falls behind `main`, use "Update branch" (or
`git rebase main`).

| Check | Required | What it runs |
|---|---|---|
| `test` | yes | `gofmt` (no unformatted files), `go mod tidy` (no diff), build, `go vet` (also with `-tags integration`), `go test -race -cover ./...` |
| `lint` | yes | `staticcheck` (pinned version, also with `-tags integration`), `shellcheck` and the tests of `scripts/`, `goreleaser check` of `.goreleaser.yaml` |
| `integration` | yes | Downloads Nomad (pinned version, SHA256-verified), starts `nomad agent -dev`, runs `go test -tags integration -race -count=1 -v ./tests/integration/...` |
| `pr-title` | yes | The PR title matches `type(scope): subject` with the types listed below and a lowercase subject (at most 72 characters) without trailing period |
| `govulncheck` | no | Known vulnerabilities in dependencies, on PRs, on `main` and weekly. Not required so a new advisory cannot block unrelated PRs |

CodeQL (default setup), secret scanning with push protection, and Dependabot
(Go modules, GitHub Actions and the Dockerfile's base image, weekly, titled
`build(deps): ...` and `ci(deps): ...`) are enabled on the repository.

Supply-chain rules: every GitHub Action is pinned to a full commit SHA (the
repository enforces it), and the workflow token is read-only. Dependabot
proposes the SHA bumps. The versions of `staticcheck`, `govulncheck` and
goreleaser are pinned in the workflows and bumped by hand. The workflow token
is read-only everywhere except the `release` job (see [Releasing](#releasing)).

To run locally what CI runs, use the commands in [Commands](#commands), plus:

```sh
test -z "$(gofmt -l .)" && go mod tidy && git diff --exit-code go.mod go.sum
go vet -tags integration ./...
```

## Releasing

A release is a `vX.Y.Z` tag on a commit of `main`. There is no image of
`main` itself.

**Versions.** nops is at `v0`. While the major is 0, **a minor may break**
(configuration, database, behaviour) and **a patch never does**; from `v1`,
plain semver (a major breaks, a minor adds, a patch fixes). The maintainer
decides when nops is stable enough for `v1`. A breaking change in a `v0`
minor is named in the release notes.

**Cutting one:**

1. `main` is green, nothing you want in it is still open (`gh pr list`) and
   the [doc audit](#doc-audit) of what changed since the last tag is done.
2. Run `scripts/release.sh`. It looks only at `origin/main`: it lists the
   commits since the last tag (breaking, features, fixes, the rest), checks
   that CI is green on the commit, and **suggests** a version, which you
   confirm or change (patch, minor, major, release candidate or one you type).
   Then it signs the tag with your GPG key and pushes it. `--dry-run` only
   shows the list and the suggestion: nothing is tagged, no prompt.
   The suggestion follows the rules above: at `v0` a release with only `fix`,
   `docs`, `ci`, `chore`, `build`, `test` and `style` commits is a patch,
   anything else (a `feat`, a `refactor`, a `perf`, a `revert`, a breaking
   change) is a minor; from `v1`, `!` is a major, `feat` a minor, the rest a
   patch. A commit that does not follow the Angular style counts as `other`.
3. A risky change (a migration, a renamed option) goes out as a release
   candidate first, `v0.2.0-rc.1` (the script's "rc" choice): it gets only its
   exact tag, is marked a prerelease on GitHub and does not move `0.2` or
   `latest`. Try it on the cluster, then tag the final one.
4. What the script does, if you ever need it by hand: tag the commit of
   `origin/main`, not a local `main` that may be behind:

   ```sh
   git fetch origin
   git tag -s v0.2.0 -m v0.2.0 origin/main
   git push origin v0.2.0
   ```

5. Watch the run (`gh run watch`), then check the result:
   `docker run --rm ghcr.io/music-gang/nops:0.2.0 -version` and the Release
   page.
6. If the release breaks something, add an "Upgrade notes" section by hand
   at the top of the Release (what to change in the job or the database).
7. Update the image in nops's own Nomad job.
8. If it goes wrong, **never move or recreate a tag** (the ruleset below
   forbids it): go back to the previous image in the job and cut a patch.

A breaking change is marked with `!` before the colon in its commit subject
(`feat(config)!: rename -git-url`) **and** carries a `BREAKING CHANGE:` footer
(see [Commit messages](#commit-messages)). The changelog lists the `!`
commits first, under *Breaking changes*, whatever their type, but reads only
the subject: the footer does not change where a commit goes, it is what
explains the break to the person upgrading. The maintainer writes the final
subject and footer at merge time.

The `release` workflow (`.github/workflows/release.yml`) then:

1. refuses a tag whose commit is not on `main`;
2. builds everything with goreleaser (`.goreleaser.yaml`) as a snapshot,
   without publishing, and runs the image's `-version`, which must print the
   tag;
3. runs the real release: the `linux/amd64` binary as a `tar.gz`,
   `checksums.txt` and a changelog from the commit subjects (*Breaking
   changes*, *Features*, *Fixes* and an *Other* group for what remains, such as
   `refactor`, `perf` and `revert`; `docs`, `ci`, `chore`, `build`, `test` and
   `style` are left out) go on the GitHub Release, and the image goes to
   `ghcr.io/music-gang/nops`.

It is the only job with write permissions (`contents` for the Release,
`packages` for GHCR), and logs in to GHCR with its own `GITHUB_TOKEN`.

**Who can tag.** Two rulesets cover the tags `v*`: "release tags: creation"
lets only a repository admin create one (a bypass for the admin role, nobody
else and no workflow), and "release tags: immutable" forbids updating or
deleting one, with no bypass at all: a published version is never rewritten.
A ruleset cannot check that the tagged commit is on `main`; the workflow does
(step 1 above). If a tag was pushed by mistake and its workflow failed
before publishing, an admin has to lift the ruleset to remove it, which is on
purpose.

**Image tags** for `vX.Y.Z`: `X.Y.Z`, `X.Y`, `latest` and, from `v1` only,
`X`. A major-only tag at `v0` would move across breaking minors, so it is not
published. A prerelease (`v0.2.0-rc.1`) gets only its exact tag.

**The image** (`Dockerfile`) is `gcr.io/distroless/static-debian13:nonroot`,
pinned by digest, with the binary goreleaser built copied in: the CA bundle,
tzdata and the `nonroot` user (uid 65532), nothing else (no shell, no `git`,
so a `file://` repository URL does not work in it). The working directory is
`/home/nonroot`, writable, so a throwaway `docker run` works with the default
`-db-path`. The Go toolchain is the `go` line of `go.mod`, installed by the
SHA-pinned `setup-go`: nothing is compiled inside the image.

**The version** comes from the tag, linked at build time:
`-ldflags "-X github.com/music-gang/nops/internal/version.version=<tag>"`
(`internal/version`). A build without it reports what the Go toolchain
recorded: the tag for `go install ...@vX.Y.Z`, a pseudo-version
(`v0.0.0-<date>-<commit>`, with `+dirty` for uncommitted changes) for a
`go build` in a checkout. It shows in `nops -version`, the `starting` log
line, the dashboard's footer and the `/healthz` body. The integration tests
build the binary with the same `-X` flag.

**Trying it locally**, with Docker running:

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$PWD":/src -w /src \
  -e GOFLAGS=-buildvcs=false --entrypoint sh goreleaser/goreleaser:v2.18.2 \
  -c 'git config --global --add safe.directory /src && goreleaser release --snapshot --clean'
docker run --rm ghcr.io/music-gang/nops:<snapshot version>-amd64 -version
```

**Bumped by hand:** the goreleaser version (in `release.yml` and `ci.yml`,
together), and the Debian release of the base image (`static-debian13` to a
future `static-debian14`: a different image name, which Dependabot does not
propose; it only bumps the digest).

**Once, after the first release:** check that the GHCR package is linked to
the repository (the `org.opencontainers.image.source` label does it) and
public.

## Commit messages

[Angular style](https://github.com/angular/angular/blob/main/contributing-docs/commit-message-guidelines.md)
(Conventional Commits):

```
<type>(<scope>): <subject>

<body: what changed and why>

<footer: BREAKING CHANGE, issue references, Co-Authored-By>
```

- **Types:** `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
  `build`, `ci`, `chore`, `revert`.
- **Scope** (optional): the package or area, e.g. `meta`, `store`, `engine`,
  `nomadx`, `hooks`, `web`, `gitwatch`, `config`, `notify`, `docs`, `examples`.
- **Subject:** imperative, lowercase, no trailing period, at most 72
  characters (`feat(store): add hook_runs table`).
- **Body:** wrap at 72 characters; plain prose, no markdown headings (this is
  what `git log` shows); explain what and why, not how; no superlatives. A
  branch can carry several commits — see [Workflow and CI](#workflow-and-ci)
  for how the maintainer turns them into the one commit that lands on
  `main`, and how the PR description, a separate and richer text, relates to
  it.
- **Breaking changes** (state machine, schema, HCL meta syntax, flags and
  environment variables): **both** `!` after the scope, and a
  `BREAKING CHANGE:` footer saying what breaks and what to do about it. The
  `!` is what puts the commit under *Breaking changes* in the release
  changelog, which reads only the subject
  ([Releasing](#releasing)); the footer is what a person reads there.
- **Attribution:** a commit made with an assistant's help carries a
  `Co-Authored-By` trailer, and nothing else attribution-wise: no session link
  or URL, no "Generated with ..." line, in a commit or in a PR description.
  `Co-Authored-By` alone says who or what wrote it.
- One logical change per PR. Code and the docs describing it go in the
  **same** PR (see the rules in CLAUDE.md).

## Go conventions

- Go version: whatever `go.mod` says. Format with `gofmt`/`goimports`. Lint: `go vet` + `staticcheck`.
- Package names are short, singular, without underscores. No `util`/`common`
  packages. Identifiers use the terms in [vocabulary](vocabulary.md).
- `context.Context` is always the first argument of any function that does I/O.
- Errors: wrap with `fmt.Errorf("dispatch hook %s: %w", id, err)`. Sentinels or
  types only when the caller needs to tell them apart (e.g. `ErrCASConflict`,
  `ErrHookTimeout`).
- Interfaces are defined by the **consumer** and kept small: `engine` declares
  what it needs from Nomad and from the store.
- No `init()` with side effects and no global state. Time is injected
  (`func() time.Time`) so timeouts are testable.
- Logging: structured `log/slog` (keys in [error-handling](error-handling.md)).
- Every flag has its `NOPS_<NAME>` env var. All intervals and timeouts are
  configurable, nothing hardcoded.
- Dependencies: before writing a library, look for an established package
  (`hashicorp/nomad/api`, `go-git`, `modernc.org/sqlite`, `oklog/ulid`).
