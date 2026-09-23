# gitwatch design

The plan for `internal/gitwatch`, agreed before writing the code (the
[`gitwatch`](../roadmap.md#gitwatch) task). gitwatch keeps the repository of
job files in memory, read-only (invariant 5 in
[philosophy](../philosophy.md)), and tells the engine which files exist at
which commit. It does not parse HCL and does not decide what a file is: that
is the engine's job, through Nomad.

## Which files are read

- Every file named `*.nomad.hcl` or `*.nomad` (the older name that
  `nomad job init` used), in any subdirectory of `-git-path` (default: the
  repository root).
- `<name>` is the file name without `.nomad.hcl` or `.nomad`. The **vars
  file**, optional, is `<name>.vars.hcl` in the same directory:
  `api.nomad.hcl` or `api.nomad` ↔ `api.vars.hcl`. When both `api.nomad` and
  `api.nomad.hcl` exist, they are two job files sharing one vars file.
- Every other file (other `.hcl`, Terraform, Packer, README…) is ignored
  without a log line. A `*.vars.hcl` with no job file of the same `<name>`
  is logged at WARN, once per commit.

### What the vars file is for

A Nomad job can declare HCL2 variables (`variable "image_tag" {}`) and use
them (`var.image_tag`). The CLI takes their values from
`nomad job run -var-file=...` or `-var`. nops runs no CLI: it sends the job
text to `/v1/jobs/parse`, which takes the values in its `Variables` field
(see [meta keys](../meta-keys.md#syntax-and-parsing)). The vars file is the
convention that tells nops which values to send.

It is optional. Without it, every variable needs a default, otherwise the
parse answers `Unset variable` and the job is skipped with an ERROR. A job
with literal values and no variables does not need one. The usual case is a
generic job whose changing values (image tag, count, domain) live in a small
file, for example one that Renovate bumps. The file is in git, so it never
holds a secret: secrets come from Nomad Variables or Vault through a
`template`.

## Job, hook or neither: decided by content

gitwatch returns files, not jobs. The engine parses each one through Nomad
(`nomadx.ParseHCL`, the only HCL interpreter) and classifies it by its meta:

| Parsed meta | What it is |
|---|---|
| `nops_role = "hook"` | a hook job |
| `nops_managed = "true"` | a managed job |
| neither | ignored |

A hook named by `nops_pre_hook` / `nops_post_hook` is looked up by its
**parsed job ID** in the same snapshot, never by file name. Two files that
parse to the same job ID are both ignored, with an ERROR: the conservative
reading, since nops cannot tell which one is meant.

## Interface

```go
type Options struct {
    URL, Branch, Path string
    Username, Token   string        // empty token: public repository
    PollInterval      time.Duration
}

type File struct {
    Path     string // repository-relative, e.g. "apps/api.nomad.hcl"
    Content  string
    VarsPath string // "" when there is no <name>.vars.hcl
    Vars     string
}

type Snapshot struct {
    Commit string // SHA the files come from
    Files  []File // sorted by Path
}

func New(o Options, log *slog.Logger) *Watcher
func (w *Watcher) Start(ctx context.Context) error // initial clone; an error is fatal for wiring
func (w *Watcher) Run(ctx context.Context)         // fetch on every tick or Trigger, until ctx ends
func (w *Watcher) Trigger()                        // non-blocking send on a channel of size 1: bursts coalesce
func (w *Watcher) Snapshot() Snapshot              // the last good snapshot
func (w *Watcher) Changed() <-chan struct{}        // size 1, signalled when the commit changes
```

`web` calls `Trigger` from the git webhook. The engine declares its own small
interface over `Snapshot` and `Changed`, and runs detection when `Changed`
fires and on the drift interval.

## Fetching

With go-git (`github.com/go-git/go-git/v5`, Apache-2.0):

- On every tick or trigger, list the remote refs (`remote.List`, cheap) and
  compare the branch head with the snapshot's commit. Nothing more happens
  when they match.
- When they differ, make a **fresh shallow clone**: depth 1, single branch,
  `NoCheckout`, in-memory storage. Read the tree of that commit, build the new
  snapshot, drop the old storage. Memory stays constant, and a force-push is
  just a new head.
- Auth: `http.BasicAuth{Username, Password: Token}` when a token is set
  (`-git-username`, `-git-token-file` / `NOPS_GIT_TOKEN`). The token is never
  part of the URL, so it never appears in an error or a log line.

## Failures

- **Initial clone** fails: `Start` returns the error and nops exits. There is
  nothing to work on.
- **Later fetch** fails (network, expired token): ERROR with the cause, keep
  the last good snapshot, retry at the next tick. Detection keeps running on
  that snapshot, so drift on the Nomad side is still seen.
- **`-git-path` missing** in a new commit: treated as a failed fetch, never
  as "every job was removed".

## Tests

Against a **local bare repository** in `t.TempDir()`, created and pushed to
with go-git over a `file://` URL, with no network:

- initial clone and snapshot content;
- a new commit is seen and `Changed` fires once; no change, no signal;
- force-push;
- triggers coalesce;
- vars pairing for `.nomad.hcl` and `.nomad`, orphan vars file (WARN);
- `-git-path` scoping; unrelated files ignored;
- a failed fetch keeps the snapshot; `-git-path` missing;
- a failed initial clone returns an error.

Auth: an `httptest` server checks the Basic auth header on the first
`info/refs` request.

## Configuration

A new option, `-git-path` / `NOPS_GIT_PATH`: the subdirectory holding the
jobs, default empty (the repository root), relative, without `..`. It goes in
[configuration](../configuration.md) with the code.
