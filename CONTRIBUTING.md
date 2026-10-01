# Contributing to Nops

The full guide is [docs/development.md](docs/development.md). In short:

1. Read the [invariants](docs/philosophy.md#invariants): a change never
   breaks them.
2. Pick an open [issue](https://github.com/music-gang/nops/issues) with no
   branch or pull request yet. Settle open design questions in the issue first.
3. Branch from `main` as `type/short-description`, for example `feat/pause-job`.
4. Add tests for every behaviour change and update the docs in the same pull
   request.
5. Run the [checks](docs/development.md#commands).
6. Write commits and the pull request title as
   [Conventional Commits](docs/development.md#commit-messages). Fill in the
   pull request template. After review, push new commits, never a force-push.

By contributing, you agree to license your contribution under the
[Apache License 2.0](LICENSE).
