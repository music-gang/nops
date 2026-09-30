# Contributing to Nops

Thanks for your interest. The full guide is in
[docs/development.md](docs/development.md); the short version:

1. Read [docs/philosophy.md](docs/philosophy.md). The invariants there are not
   negotiable, and some features are deliberately out of scope.
2. Pick an open [issue](https://github.com/music-gang/nops/issues) nobody is
   working on yet (no branch, no open pull request). If its design is still
   open, settle it in the issue before writing code.
3. Branch from `main` (`type/short-description`, e.g. `feat/pause-job`).
   `main` is protected: all changes go through a pull request.
4. Add tests for every behaviour change and update the docs in the same PR.
   A `feat`, a `fix` or a breaking change also adds its line under
   `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md), written for someone
   running Nops ([how](docs/development.md#changelog)).
5. Make sure the checks pass
   ([commands](docs/development.md#commands) and
   [what "done" means](docs/development.md#when-a-piece-of-work-is-done)).
6. Title the PR in [Angular style](docs/development.md#commit-messages)
   (`type(scope): subject`) and write each commit of the branch in the same
   style. PRs are squash-merged: the title becomes the commit subject on
   `main` and the body comes from the branch's commits, which the maintainer
   checks by hand when merging. The PR description is a separate text for the
   reviewer (`## What changes`, `## Why`, and a `## Notes` if there is a
   question, plus `Closes #N`) and does not reach `main`; the PR template has
   those headings. A change after review is a new commit, not a force-push.
   Details in [docs/development.md](docs/development.md#workflow-and-ci).

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
