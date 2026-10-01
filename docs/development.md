# Development and testing

## What needs which tests

| Area | Kind of test |
|---|---|
| `internal/meta` | Table-driven unit tests: valid values, invalid values, unknown keys, defaults. |
| `internal/engine` | Table-driven unit tests for **every** transition, including crash recovery and CAS conflicts, with an in-memory fake Nomad and an injected clock. Every invariant has an **adversarial** test, one that tries to break it instead of walking the path beside it (an apply step after every write of detection, a store write failing at every point of a deployment's life, a live job edited between the plan and the register): `invariants_test.go`, and the *Proven by* lines of [philosophy](philosophy.md#invariants). A new one is checked by breaking the code it guards and seeing it fail. |
| `internal/nomadx` | Unit tests against an `httptest` stub of the Nomad API (request shape, error mapping) + integration tests against a real Nomad. |
| `internal/store` | Against **real SQLite** on a temporary file (`t.TempDir()`), not mocks: constraints, the lock index, migrations. |
| `internal/hooks` | Unit tests with an in-memory fake Nomad, a **real SQLite** store and an injected clock (success, failure, timeout, resume, redispatch with token, Nomad/SQLite errors) + integration with `raw_exec` hooks. |
| `internal/redact` | Unit: no secret must reach the DB or the HTML, over hand-built diffs and a fixture from a real plan (`testdata/plan_diff.json`) + integration: the same rules on a live plan. |
| `internal/notify` | `httptest` receivers: the request of every adapter, several adapters at once, and non-2xx, timeout, unreachable and cancelled (a WARN each, no error, no URL in the log). |
| `internal/gitwatch` | Unit tests against a local bare repository over `file://` (new commit, no change, force-push, coalesced triggers, vars pairing, `-git-path` scoping, a failed fetch keeps the snapshot). Skipped if `git` is not on `PATH`. |
| `internal/web` | `httptest` for handlers, with fakes for the store, the engine and git and a fixed clock: every page in every state it has (empty, pending, blocked, failed, invalid meta, a store error), the helpers (relative times, the diff summary, sync state, the plan steps), that the whole `spec_hash` is in the approve form and `job_spec` never on a page; the login against a fake OIDC provider (`httptest` serving discovery, keys, token and userinfo, signing real ID tokens), cross-origin refusals and 401/403. No coverage target. |
| `cmd/nops` | Almost entirely straight-line wiring, so almost entirely covered by the integration smoke test below, not unit tests: a unit test only for the one piece of actual logic (`newAuthenticator` picking the login backend by `-auth-mode`). No coverage target. |
| The docs | Tests inside `go test ./...` that read the pages: the option tables of `configuration.md` and the key table of `meta-keys.md` against `internal/config` and `internal/meta` (both directions, defaults included), and `docs/docs_test.go`, which fails when a test named in any `.md` (a name ending in `*` is a group) is not a function in the repo, and when the README names a Nomad version other than `NOMAD_VERSION` in `ci.yml`; the same file checks every relative link and anchor, and that the docs speak to people ([below](#docs-and-agent-instructions)). They prove the page and the code agree on names and defaults, not that a cited test asserts the claim. |
| `scripts/release.sh` | `scripts/release_test.sh` (plain bash, run by the `lint` job) covers the pure functions: the suggested bump, the next version, the release candidate number, semver validation and order. `shellcheck` on every script. The flow around them (git, `gh`, the questions it asks) is looked at with `--dry-run`, never in CI: it would tag. |
| Real interaction with Nomad | Integration. |

## Integration

- Build tag `integration`, in `tests/integration/`. They run against a local
  `nomad agent -dev`. If `NOPS_TEST_NOMAD_ADDR` is not set they are skipped
  (skip, not fail).
- What the Nomad token needs (`acl_test.go`, the tables of
  [token ACL](running-nops.md#the-nomad-token)) runs against a second dev agent with
  ACLs enabled: `NOPS_TEST_NOMAD_ACL_ADDR` is its address and
  `NOPS_TEST_NOMAD_ACL_TOKEN` a management token (the bootstrap one). The tests
  create their own policies and tokens and delete them; without the two
  variables they are skipped.
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
  real dump restores, the cluster's own ACLs and TLS) is judged by using Nops
  on the cluster.
- The **end-to-end tests** (`e2e_*_test.go`) build the real `nops` binary once
  and run it as a subprocess against a scratch git repository (a bare repo
  over `file://`, pushed to by the test) and the Nomad under test: basic
  auth, 200ms intervals, the dashboard driven over HTTP and Nops's own SQLite
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

### Nomad version

The version the integration tests run on is `NOMAD_VERSION` in
`.github/workflows/ci.yml`, with the SHA-256 of the linux/amd64 zip from
`https://releases.hashicorp.com/nomad/<version>/nomad_<version>_SHA256SUMS`.
It is the only place a version is set: the README states it in its *Nomad
compatibility* section (`docs/docs_test.go` fails if the two differ), and the
other pages and the code comments name the test that checks a behaviour, not a
version. The archived decision log is the exception: a row says what was true
the day it was written.

To move to a new Nomad: change `NOMAD_VERSION` and `NOMAD_SHA256`, update the
README line, read the release notes between the two versions for the API
behaviours listed above, and run the integration tests on a throwaway agent of
the new version. The fixture `internal/redact/testdata/plan_diff.json` stays
the plan captured when it was written; capture it again only if a new Nomad
changes a field name and the redaction has to follow.

### First rollout on a real cluster

The real check is using it. The policies allow going in steps: start with
`nops_policy = "none"` on a few jobs and watch `/drift`; move stateful jobs to
`approval` and stateless ones to `auto` one at a time.

## Invariant tests

Each [invariant](philosophy.md#invariants) has tests that try to break it, and
what is deliberately not tested is said too.

| Invariant | Tests |
|---|---|
| Plan before every write | `TestEveryStoreWriteOfADeploymentsLifeCanFailOnceWithoutBreakingTheOrder` (every register has a plan of the same job in the same step, with no write in between), `TestStepRegisterEmptyPlanCompletes`, `TestAHookRevisionIsNotRegisteredWhenItsPlanFails`. |
| CAS on every register | `TestARegisterUsesTheIndexCapturedAtDetection` (the captured index, and a live job edited between the plan and the register), `TestRegisterCASAlwaysEnforcesIndex` (the wrapper), `TestPlanAndRegisterCAS` and `TestE2EAutoRevertsOutsideEdit` (against a real Nomad). |
| Never auto-apply under policy `approval` | `TestAnApplyStepAfterEveryWriteOfDetectionNeverAppliesUnderApproval` (an apply step after every write of detection), `TestStepDetectedNeverAdvancesADeploymentUnderApproval`, `TestRecoveryLeavesPendingApprovalAlone`, `TestApproveRefusesStaleSpecHash`, `TestApprovePassesOnTheHashOfTheFormNotTheStoredOne`, `TestPagesRequireLogin`. |
| Git is the source of truth for Nops's behaviour | `TestAnInvalidMetaKeyIsReadFromGitAsPolicyNoneWhateverTheLiveJobSays` (a typo, or another invalid key next to a valid policy; the live job's meta is never read), `TestAnInvalidKeyIntroducedIntoAPendingApprovalSupersedesIt`, `TestTheLivePolicyIsNeverWhatDecidesWhetherToDeploy`, `TestParse`. |
| A person's pause (under 4) | `TestPausedJobGetsNoDeployment`, `TestPauseLeavesAPendingApprovalPending`, `TestPauseLeavesADeploymentInFlightAlone`, `TestResumeLetsTheNextCycleDeployAsUsual`. |
| Nops never writes to Git | `TestDetectionStoresTheSpecExactlyAsGitHasIt`, `TestStepRegisterRegistersTheSpecUnchanged`, `TestAHookRevisionIsRegisteredWithTheMetaOfItsSpec`. That Nops never writes to Git is structural, not tested: the clone lives in memory and nothing in the code pushes. |
| One active deployment per job | `TestPerJobLock` (against real SQLite: every active state holds the lock, a terminal one releases it), `TestALockHeldByTheDatabaseSkipsTheJobAndNotTheCycle`. Not tested: two creations at the same time. |
| State is persisted before acting | `TestEveryStoreWriteOfADeploymentsLifeCanFailOnceWithoutBreakingTheOrder` (a store write fails once at every point of a life: no write to Nomad before the one that authorizes it, the job registered once, the deployment still completes), `TestAFailedIntentWriteLeavesNomadUntouched`, `TestRunMarkerSaveErrorSendsNothing` (hooks), `TestGCStoreFailureIsReturned`. |

## Coverage

≥ 80% on `engine`, `store`, `meta`, `hooks` (`go test -cover`). No target on
`web` and `cmd`.

## Doc audit

The tests above catch names and defaults that drift. What they cannot catch is a
claim that was never true, or stopped being: an invariant, a transition, what a
page says a test covers. A **doc audit** checks those, and is done before a
release (`scripts/release.sh` reminds you) or when someone asks for it. The code
and its tests are the evidence; the docs and their cross-references are only
the claims being checked.

**Scope.** The pages and packages changed since the last tag:
`git diff --stat <last-tag>..HEAD -- '*.md' cmd internal scripts examples .github`.
The first audit, or one with no tag, covers everything.

**What it does:**

1. **Inventory the claims** of the pages in scope, plus the pages that describe
   a package in scope: invariants and state transitions, defaults, flag and env
   names, meta keys, errors and notifications, what the dashboard shows, what a
   test is said to cover, and the reason a page gives for a design choice.
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
   behaviour change in its own, each with a test that fails before the fix, its
   reason in the page that explains that part.

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

## If you change X, update Y

A fact is written in one place and linked from the others; this table says
which place.

| Change | Update |
|---|---|
| New meta key or different valid values | `internal/meta` **and** `meta-keys.md` (its table is tested against the keys) |
| Behaviour of a policy | `policies.md` |
| Hook contract or dispatch meta | `hooks.md` and the example in `examples/` |
| A scenario in `examples/` | `TestExamplesParse` checks every example; a new scenario needs nothing else unless it shows new behaviour worth an end-to-end test |
| State, transition, schema, recovery | `deployment-lifecycle.md` |
| Detection cycle (parse, plan, create, supersede, freeze hooks, revision GC, orphans) | `internal/engine` **and** `architecture.md#detection` (and `#hook-revisions`) |
| Apply loop (approve, reject, register, health, timeouts, promotion, retry) | `internal/engine` **and** `architecture.md#apply` |
| Which files are read from git, the git watcher | `internal/gitwatch` **and** `architecture.md#reading-the-repository` |
| A dashboard page, route, sync state or login | `internal/web` **and** `dashboard.md` |
| What is logged, or which transition notifies | `logs-and-notifications.md` (the one place that says it: other pages link to it) |
| What running Nops on a cluster needs (the Nomad job, the token's ACL, a proxy, the OIDC client, the git webhook) | `running-nops.md` (its token table is checked by `TestTokenACLForVolumes`) |
| The first steps on one machine | `getting-started.md`, run again by hand when a step it shows changes |
| Flag or env var | `internal/config` **and** `configuration.md` (its tables are tested against the options: name, variable, default) |
| Notification adapter or payload | `internal/notify` **and** `logs-and-notifications.md#notifications` |
| A test is renamed or deleted | every page that names it (`docs/docs_test.go` fails otherwise) |
| A page or a heading is renamed or moved | every link to it (`TestRelativeLinksResolve` fails otherwise) |
| The Nomad version CI tests against | `ci.yml` (`NOMAD_VERSION`, `NOMAD_SHA256`) **and** the README's *Nomad compatibility* (`docs/docs_test.go` fails otherwise); steps in [Nomad version](#nomad-version) |
| New concept, or a renamed term | `glossary.md`, or [Terms used in the code](#terms-used-in-the-code) for one that only matters in the code |
| A design decision | its reason, in a sentence or two, in the page that explains that part; the discussion stays in the issue or the PR |
| Build or release (`Dockerfile`, `.goreleaser.yaml`, `release.yml`, `scripts/`), image tags, semver policy | [Releasing](#releasing) |
| Invariant | `philosophy.md`, and its tests in [Invariant tests](#invariant-tests) (`TestInvariantTitlesMatch` fails when a copy of the list elsewhere drifts) |

## Docs and agent instructions

The docs, the README and the contributing guide describe Nops and how anyone
contributes. What only a coding agent needs (how it works in a session, how it
reports, how it marks what it wrote) lives in the file for agents at the root
of the repository, which links to these pages instead of repeating them.
`TestDocsSpeakToPeople` fails when a page other than that file names an agent
tool or talks to one; `TestInvariantTitlesMatch` keeps that file's short list
of invariants the same as [philosophy](philosophy.md). The first checks words,
not tone: it was added because a written rule alone kept being broken, and
every line that slipped through named the tool or the session.

## Commands

```sh
go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest -tags integration ./...

nomad agent -dev &                     # in another terminal
NOPS_TEST_NOMAD_ADDR=http://127.0.0.1:4646 go test -tags integration -race -count=1 ./tests/integration/...

# the ACL tests: a second agent, with ACLs, on other ports
printf 'ports {\n  http = 5646\n  rpc  = 5647\n  serf = 5648\n}\nacl {\n  enabled = true\n}\n' > acl-agent.hcl
nomad agent -dev -config=acl-agent.hcl &
export NOPS_TEST_NOMAD_ACL_ADDR=http://127.0.0.1:5646
export NOPS_TEST_NOMAD_ACL_TOKEN=$(curl -s -X POST $NOPS_TEST_NOMAD_ACL_ADDR/v1/acl/bootstrap | jq -r .SecretID)
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

**The flow of a change:**

1. The work is the open issues. Pick one that has no branch and no open PR
   yet (`git ls-remote --heads origin`, the pull requests page). An issue
   whose proposal still leaves a design choice open is settled in the issue
   first: no branch and no PR for a plan, since a PR is something to review.
2. Branch from an up-to-date `main`, named `type/short-description`
   (`feat/pause-job`), the type as in [Commit messages](#commit-messages).
3. Make the change with its tests and docs
   ([When a piece of work is "done"](#when-a-piece-of-work-is-done)), the
   reason for a design decision in the page that explains that part.
4. Commit in [Angular style](#commit-messages). A branch can carry several
   commits, each a plain, honest step (what changes and why, no
   superlatives): the maintainer squashes them and writes the commit that
   lands on `main` by hand at merge time.
5. Open a PR whose **title** is an Angular-style message
   (`type(scope): subject`): it becomes the subject on `main`, and a line of
   the next release notes ([Commit messages](#commit-messages)). Its
   **description** is for the reviewer and never reaches `main`: the
   template's headings below, and `Closes #N` for the issue it finishes. A
   question left to the maintainer goes in its `## Notes`.
6. A change after the PR is open (a fix, an answer to review) is a **new
   commit** on the same branch, never an amend or a force-push of what is
   already pushed.
7. Only the maintainer merges.

What's specific to this repo's CI and merge mechanics:

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

It is only the three headings (`## Notes` is optional: delete it when there
is nothing to say), each section a short bullet list filled in by hand, never
left as a placeholder, with no HTML comment, no boilerplate and no unchecked
checklist item (see the
decision log, 2026-09-24: an earlier template leaked its HTML comments and
an unfilled checklist into commit bodies, `a933a4c` and `8052989`, back when
the squash setting copied the PR description verbatim). The template
applies to a PR opened from the GitHub web page; `gh pr create --body-file`
replaces it. The "done" checklist stays in
[When a piece of work is "done"](#when-a-piece-of-work-is-done), not in the
PR body. If the branch falls behind `main`, use "Update branch" (or
`git rebase main`).

Issues go through the two forms in `.github/ISSUE_TEMPLATE/` (blank issues
are disabled). An issue's title is plain text ("Retry a failed deployment
from its page"): the Angular style is for commits and PR titles. The kind of
an issue is its type: the bug form sets `Bug`, the feature form `Feature`,
and the rest is a `Task`. Both forms add the `needs-triage` label; the
maintainer removes it once he has read the issue and adds a label for each
area it touches: `engine`, `web`, `store`, `meta`, `nomadx`, `hooks`,
`gitwatch`, `config`, `notify`, `integration`, `release`, `ci` or
`documentation`; a unit test belongs to the area of its package. An issue
opened with `gh issue create` skips the forms and gets no type and no label.
PRs have no labels, since their title already says the type and the scope,
except `dependencies` and `go`, which Dependabot adds. The open issues are the
list of work: there is no roadmap file.

| Check | Required | What it runs |
|---|---|---|
| `test` | yes | `gofmt` (no unformatted files), `go mod tidy` (no diff), build, `go vet` (also with `-tags integration`), `go test -race -cover ./...` |
| `lint` | yes | `staticcheck` (pinned version, also with `-tags integration`), `shellcheck` and the tests of `scripts/`, `goreleaser check` of `.goreleaser.yaml` |
| `integration` | yes | Downloads Nomad (pinned version, SHA256-verified), starts `nomad agent -dev` and a second one with ACLs (bootstrapped, its token masked), runs `go test -tags integration -race -count=1 -v ./tests/integration/...` |
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

**Versions.** Nops is at `v0`. While the major is 0, **a minor may break**
(configuration, database, behaviour) and **a patch never does**; from `v1`,
plain semver (a major breaks, a minor adds, a patch fixes). The maintainer
decides when Nops is stable enough for `v1`. A breaking change in a `v0`
minor is named in the release notes.

**Cutting one:**

1. `main` is green, nothing you want in it is still open (`gh pr list`) and
   the [doc audit](#doc-audit) of what changed since the last tag is done.
2. Run `scripts/release.sh`. It looks only at `origin/main`: it lists the
   commits since the last tag (breaking, features, fixes, the rest), checks
   that CI is green on the commit, and **suggests** a version, which you
   confirm or change (patch, minor, major, release candidate or one you type).
   Then it signs the tag with your GPG key and pushes it. `--dry-run` only
   shows the list and the suggestion: nothing is tagged, nothing is asked.
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
7. Update the image in Nops's own Nomad job.
8. If it goes wrong, **never move or recreate a tag** (the ruleset below
   forbids it): go back to the previous image in the job and cut a patch.

A breaking change is marked with `!` before the colon in its commit subject
(`feat(config)!: rename -git-url`) **and** carries a `BREAKING CHANGE:` footer
(see [Commit messages](#commit-messages)). The release notes list the `!`
commits first, under *Breaking changes*, whatever their type, but read only
the subject: the footer does not change where a commit goes, and does not
reach the notes, so what to do when upgrading goes in the Release's
*Upgrade notes* (step 6 above). The maintainer writes the final subject and
footer at merge time.

The `release` workflow (`.github/workflows/release.yml`) then:

1. refuses a tag whose commit is not on `main`;
2. builds everything with goreleaser (`.goreleaser.yaml`) as a snapshot,
   without publishing, and runs the image's `-version`, which must print the
   tag;
3. runs the real release: the `linux/amd64` binary as a `tar.gz`,
   `checksums.txt` and the release notes go on the GitHub Release, and the
   image goes to `ghcr.io/music-gang/nops`. The notes are the subjects of the
   commits since the previous tag, one line each without its SHA, grouped as
   *Breaking changes*, *Features*, *Fixes* and *Other* (such as `refactor`,
   `perf` and `revert`); `docs`, `ci`, `chore`, `build`, `test` and `style`
   commits are left out. That is why a subject is written for someone running
   Nops ([Commit messages](#commit-messages)).

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
  characters (`feat(store): add hook_runs table`). The subject of a squash
  commit on `main` is a line of the next release notes (unless its type is
  `docs`, `ci`, `chore`, `build`, `test` or `style`), so it says what changes
  for someone running Nops, not how the code changed:
  `feat(web): pause a job from the dashboard (#88)`, not
  `feat(engine): add a hold gate to stepDetected`.
- **Body:** wrap at 72 characters; plain prose, no markdown headings (this is
  what `git log` shows); explain what and why, not how; no superlatives. A
  branch can carry several commits — see [Workflow and CI](#workflow-and-ci)
  for how the maintainer turns them into the one commit that lands on
  `main`, and how the PR description, a separate and richer text, relates to
  it.
- **Breaking changes** (state machine, schema, HCL meta syntax, flags and
  environment variables): **both** `!` after the scope, and a
  `BREAKING CHANGE:` footer saying what breaks and what to do about it. The
  `!` is what puts the commit under *Breaking changes* in the release notes,
  which read only the subject ([Releasing](#releasing)); the footer is for
  `git log`, and what to do when upgrading also goes in the Release's
  *Upgrade notes*.
- **Co-author:** someone who wrote part of a commit is named in a
  `Co-Authored-By` trailer.
- One logical change per PR. Code and the docs describing it go in the
  **same** PR.

## The dashboard's code

How the pages are built; what they show is in [dashboard](dashboard.md).

### Look and technology

No JS framework, no build step: `html/template` renders every page (all
templates parsed once at startup, so a broken one fails loud rather than on
the first request), plain CSS carries the design, and
[htmx](https://htmx.org) is the only script, vendored under `/static` rather
than loaded from a CDN. Every action works as a plain form post without it;
htmx adds `hx-boost` (page navigation without a full reload) and the polling
below. The CSP is `script-src 'self'; style-src 'self'` — there is no inline
script or style to allow.

The logo is the ship of the logo of the [README](../README.md) (a planet with
the ship, `docs/assets/logo.webp`, which the dashboard does not embed). The
ship alone is exported as PNGs under `internal/web/static`: the header mark
(`logo-96x96.png`, shown at 40px, the only thing in the header's corner: no name
beside it), the mark above the login form (`logo-192x192.png`, shown at 96px), the favicons (`favicon-16x16.png`,
`favicon-32x32.png`) and the touch icon (`apple-touch-icon.png`, 180px). Every
page links the favicons and the touch icon, the login page too, through the same
`asset` function as the stylesheet. The login page is flat, in the manner of GitHub's:
no card, the mark, the title "Sign in to Nops" and the form on the page's own
background.

Overview, Jobs, Job and Activity poll their own address every 5s and swap in
only the region a full re-render of the same page would show
(`hx-select="#live"`, `hx-swap="outerHTML"`; the request keeps a page's own
`?state=` filter, since it is part of `.Self`), so a new deployment, a changed
sync state or the result of *Fetch now* shows up without a reload. Rendering
the whole page again costs nothing here (local SQLite and the engine's
in-memory state), so there is no dedicated fragment endpoint for them, unlike
the Deployment page's `/status` above. The Job page uses three such regions
(`#live-head`, `#live-deployments` and `#live-details`) so the drift diff
between them, whose `<details>` nodes a person may have opened or closed,
is never re-rendered by the poll.

Each of these regions carries `hx-disinherit="hx-select hx-swap"`: `hx-boost`
turns every link and form inside it (a job's link, *Retry*, *Fetch now*) into
its own boosted request, and htmx attributes are inherited by children unless
told otherwise, so without it a boosted link would pick up the region's own
`hx-select`/`hx-swap` and apply them to *its own* navigation — selecting
`#live` out of whatever page it lands on (blank, if that page has no such
element) and swapping it in with `outerHTML` over the whole body (dropping the
header and the page's width). See the [decision log](archive/decisions.md),
2026-09-25.

Static files are cached for a day, so the templates link them through the
`asset` function, which adds a version taken from the file's own bytes
(`/static/app.css?v=<10 hex digits of its SHA-256>`): a changed file has a new
address and a browser never shows the new pages with the old stylesheet, while
an unchanged one stays cached. The version is computed once per file from the
embedded copy; a file that does not exist makes the page fail loud (a logged
500) instead of linking a 404. The login page uses it too.

The look reads [GitHub's Primer](https://primer.style): its color tokens for
light and dark (the theme follows the system), a 14px base, 6px corners and
system font stacks, so nothing is downloaded. It is built for scanning, not
reading: **one thing per line**, cut with an ellipsis rather than wrapped, with
the full value on hover. On a narrow screen (768px or less) a row becomes two
lines on purpose (the state and name, then the detail), secondary columns are
hidden, and a wide diff scrolls inside its own box, never the page.
Deployment states map to one of five colors used consistently across every
page: pending (amber), running — `pre_hook`/`applying`/`post_hook` — (blue),
completed (green), failed (red), rejected/superseded (muted grey).

The plan diff renders Nomad's `JobDiff` recursively (job → task groups →
tasks → objects/fields) as nested `<details>`, open only where something
changed; a redacted value (`<redacted>`) renders as a pill rather than plain
text, so it reads as "a secret changed here" at a glance. Above it, a summary
counts the field changes (added, edited, removed) and says in which job, task
group or task they are; a redacted field counts like any other.

### Writes and CSRF

A request that changes state (`POST`) is refused with 403 when the browser says
it comes from another origin (`Sec-Fetch-Site`, or `Origin` against `Host`:
Go's `http.CrossOriginProtection`), on top of the `SameSite=Lax` cookie, for
both login backends alike — this includes `POST /auth/login` itself,
against session-fixation-style login CSRF, not only the dashboard's own
writes. There is no CSRF token to carry through the pages. Requests with
neither header (a `curl`) are allowed, and still need the session cookie
where one is required.

## Terms used in the code

The words of [the glossary](glossary.md), plus these, which only matter inside
the code or the repository.

### Code and tooling

| Term | Meaning |
|---|---|
| **Nops** / **`nops`** | **Nops** is the product, written with a capital in prose (docs, README, PR descriptions), like Nomad. **`nops`** in code font is the command: the binary, the image (`ghcr.io/music-gang/nops`), the Go module, a path (`cmd/nops`), a log or config name. `NOPS_*` variables and `nops_*` meta keys are keys and keep their spelling. The logo may be lowercase: its lettering is not the prose spelling. |
| **store** | `internal/store`: SQLite, the only place state lives. |
| **nomadx** | `internal/nomadx`: the Nomad client wrapper, CAS-only register, sentinel errors. |
| **engine** | `internal/engine`: the state machine that moves deployments forward. Its two loops are **detection** (also called the **reconciler**: parses, plans, creates/supersedes/revalidates deployments) and the **apply loop** (advances non-terminal deployments); **recovery** is the apply loop's first cycle. |
| **runner** | `hooks.Runner`: runs one hook run to a terminal state. Blocking and idempotent. |
| **sentinel error** | An exported `Err...` value the caller tests with `errors.Is` (`ErrCASConflict`, `ErrJobNotFound`, `ErrActiveDeployment`). |
| **fake / stub** | *Fake*: an in-memory stand-in with behaviour (the fake Nomad in `hooks` tests). *Stub*: an `httptest` server that returns canned answers (`nomadx` tests). |
| **integration test** | A test against a real `nomad agent -dev` (build tag `integration`). |

### Work on the repo

The mechanics behind these terms (branching, PRs, how an issue becomes a PR)
are in [development](#workflow-and-ci); this is just what to
call them.

| Term | Meaning |
|---|---|
| **PR** | Pull request. Its **title** is the commit that lands on `main`, in Angular style `type(scope): subject`. |
| **issue** | A GitHub issue: one piece of work, with its problem, its proposal and, while it is being designed, what is still to decide. The open issues are the work left. |
| **in flight** | An issue with a remote branch or an open PR. Derived from git and GitHub, never written down. |
| **plan first** | An issue whose design questions still need the maintainer's answer before any branch exists. |
| **decision log** | [`archive/decisions.md`](archive/decisions.md): the design decisions up to v0.4.0, kept as history and no longer written to. A decision's reason is now in the page that explains that part. |
| **invariant** | One of the seven rules in [philosophy](philosophy.md) that no change may break. |
| **doc audit** | Checking the claims of the docs against the code and its tests, before a release or on request; a report of findings, then fixes as agreed ([procedure](#doc-audit)). Not the tests that already compare tables and test names with the code. |
| **release** | A `vX.Y.Z` tag on `main` and what the `release` workflow publishes for it: the image on GHCR, the binary, checksums and the release notes on the GitHub Release ([releasing](#releasing)). |

## Go conventions

- Language: code, comments, docs, examples and commit messages are in English.
- Go version: whatever `go.mod` says. Format with `gofmt`/`goimports`. Lint: `go vet` + `staticcheck`.
- Package names are short, singular, without underscores. No `util`/`common`
  packages. Identifiers use the terms in [glossary](glossary.md).
- `context.Context` is always the first argument of any function that does I/O.
- Every state transition goes through a single store function
  (`Store.Transition`), which updates `deployments` and writes `events` in the
  same transaction; there are no "manual" transitions.
- Errors: wrap with `fmt.Errorf("dispatch hook %s: %w", id, err)`. Sentinels or
  types only when the caller needs to tell them apart (e.g. `ErrCASConflict`,
  `ErrHookTimeout`).
- Interfaces are defined by the **consumer** and kept small: `engine` declares
  what it needs from Nomad and from the store.
- No `init()` with side effects and no global state. Time is injected
  (`func() time.Time`) so timeouts are testable.
- Logging: structured `log/slog` (keys in [logs and notifications](logs-and-notifications.md)).
- Every flag has its `NOPS_<NAME>` env var. All intervals and timeouts are
  configurable, nothing hardcoded.
- Dependencies: before writing a library, look for an established package
  (`hashicorp/nomad/api`, `go-git`, `modernc.org/sqlite`, `oklog/ulid`).
