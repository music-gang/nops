# Development and testing

## What needs which tests

| Area | Kind of test |
|---|---|
| `internal/meta` | Table-driven unit tests: valid values, invalid values, unknown keys, defaults. |
| `internal/engine` | Table-driven unit tests for **every** transition, including crash recovery and CAS conflicts, with an in-memory fake Nomad and an injected clock. |
| `internal/nomadx` | Unit tests against an `httptest` stub of the Nomad API (request shape, error mapping) + integration tests against a real Nomad. |
| `internal/store` | Against **real SQLite** on a temporary file (`t.TempDir()`), not mocks: constraints, the lock index, migrations. |
| `internal/hooks` | Unit tests with a fake Nomad (success, failure, timeout, redispatch with token) + integration. |
| Diff redaction | Unit: no secret must reach the DB or the HTML. |
| `internal/web` | `httptest` for handlers, header auth, CSRF and 403. No coverage target. |
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
- Realistic scenarios (pre-pulling heavy images, backups) are manual
  checklists in `docs/acceptance/`, to be run on the real cluster.

## Coverage

≥ 80% on `engine`, `store`, `meta`, `hooks` (`go test -cover`). No target on
`web` and `cmd`.

## When a piece of work is "done"

A commit/PR is complete only if:

1. `go build ./...`, `go vet ./...`, `staticcheck` and `go test ./...` are green;
2. if it touches `engine`, `hooks` or `nomadx`, the integration tests are also
   green against a `nomad agent -dev`;
3. every behaviour change has its test;
4. the docs (and `examples/`) are updated if the HCL syntax, states or a
   decision change.

## Commands

```sh
go test -race -cover ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...

nomad agent -dev &                     # in another terminal
NOPS_TEST_NOMAD_ADDR=http://127.0.0.1:4646 go test -tags integration ./tests/integration/...
```

`staticcheck`: with a recent Go, the binary in `~/go/bin` may have been built
with an older Go and fail with "export data version"; `go run ...@latest`
avoids the problem.

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
- **Body:** wrap at 72 characters; explain what and why, not how. Plain
  language, no superlatives.
- **Breaking changes** (state machine, schema, HCL meta syntax): add a
  `BREAKING CHANGE:` footer, or `!` after the scope.
- One logical change per commit. Code and the docs describing it go in the
  **same** commit (see the rules in CLAUDE.md).

## Go conventions

- Go ≥ 1.22. Format with `gofmt`/`goimports`. Lint: `go vet` + `staticcheck`.
- Package names are short, singular, without underscores. No `util`/`common`
  packages.
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
