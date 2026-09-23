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
   (`type(scope): subject`). PRs are squash-merged, so the title becomes the
   commit message on `main`.

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
