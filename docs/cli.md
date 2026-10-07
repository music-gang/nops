# Command-line client

Inspect and act on Nops from a terminal. The client is the same `nops` binary
and calls the [API](api.md): it does what the token's owner can do on the
dashboard, with the same rules, and Nops records the owner as the
[actor](glossary.md#deployment-lifecycle). `nops serve` runs the server.

## Configuration

Create a token on the dashboard's *Tokens* page, as for the [API](api.md#tokens).

| Flag | Variable | Meaning |
|---|---|---|
| `-addr` | `NOPS_ADDR` | **Required.** The URL of Nops, such as `https://nops.example.com`. |
| `-token-file` | `NOPS_TOKEN_FILE` | **Required**, unless `NOPS_TOKEN` is set. File holding the token. |
| none | `NOPS_TOKEN` | The token itself. It has no flag, so it never shows in `ps`. The file wins. |
| `-namespace` | `NOPS_NAMESPACE` | The namespace of the job a command names. Default `default`. |

A flag wins over its variable. Put flags before arguments.

## Commands

A command that takes a `<job>` looks for it in the namespace you set.

| Command | What it does |
|---|---|
| `nops jobs` | List the jobs with their [sync state](dashboard.md#sync-state-of-a-job). |
| `nops job <job>` | Show a job with its drift and deployments. |
| `nops deployments` | List the active deployments and the latest finished ones. |
| `nops deployment <id>` | Show a deployment with its diff, hook runs, and events. |
| `nops approve <id>` | Show the diff, ask for confirmation, and approve. `-yes` skips the question. |
| `nops reject <id>` | Reject. |
| `nops promote <id>` | Promote the canaries of a deployment that waits for it. |
| `nops retry <id>` | Retry a failed or rejected deployment and print the new ID. |
| `nops pause <job>` | [Pause](policies.md#pausing-a-job) a job. `-reason` says why. |
| `nops resume <job>` | Resume a job. |
| `nops deploy-now <job>` | Show the drift, ask for confirmation, and [deploy now](policies.md#sync-window). `-yes` skips the question. |
| `nops fetch` | Ask for a git poll now. |
| `nops secret generate` | Print a new random secret for a file Nops reads, such as the [metrics token](metrics.md#scraping). It runs offline: it needs no `-addr` and no token. |

`approve` and `deploy-now` send the spec hash of the diff they showed, so a
newer diff is refused as on the dashboard.

## Output

Text by default. `-json` prints the answer of the API, for a script. The diff
and the question go to the error output, so a pipe gets only the answer. A
command that fails exits with 1 and prints the message of the API.
