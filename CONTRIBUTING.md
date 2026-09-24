# Contributing to nops

Thanks for your interest. The full guide is in
[docs/development.md](docs/development.md); the short version:

1. Read [docs/philosophy.md](docs/philosophy.md). The invariants there are not
   negotiable, and some features are deliberately out of scope.
2. Branch from `main` (`type/short-description`, e.g. `feat/hooks-package`).
   `main` is protected: all changes go through a pull request.
3. Add tests for every behaviour change and update the docs in the same PR.
4. Make sure `go build ./...`, `go vet ./...`, staticcheck and
   `go test -race ./...` pass (commands in `docs/development.md`).
5. Title the PR in [Angular style](docs/development.md#commit-messages)
   (`type(scope): subject`) and write each commit of the branch in the same
   style. PRs are squash-merged: the title becomes the commit subject on
   `main` and the body comes from the branch's commits, which the maintainer
   checks by hand when merging. The PR description is a separate text for the
   reviewer (`## What changes`, `## Why`, and a `## Notes` if there is a
   question) and does not reach `main`; the PR template has those headings.
   Details in [docs/development.md](docs/development.md#workflow-and-ci).

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
