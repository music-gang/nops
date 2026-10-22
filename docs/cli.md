# Command-line client

Inspect and act on Nops from a terminal. The client is the same `nops` binary
and calls the [API](api.md): it does what the token allows, with the
dashboard's rules, and Nops records the token as the
[actor](glossary.md#deployment-lifecycle). `nops serve` runs the server.

## Configuration

Log in with `nops login`, or get a token as for the [API](api.md#tokens).

| Flag | Variable | Meaning |
|---|---|---|
| `-addr` | `NOPS_ADDR` | **Required.** The URL of Nops, such as `https://nops.example.com`. |
| `-token-file` | `NOPS_TOKEN_FILE` | File holding the token. |
| none | `NOPS_TOKEN` | The token itself. It has no flag, so it never shows in `ps`. The file wins. |
| `-namespace` | `NOPS_NAMESPACE` | The namespace of the job a command names. Default `default`. |

A flag wins over its variable. Put flags before arguments.

Without a token file or `NOPS_TOKEN`, the client uses the session that
`nops login` saved for the same URL.

## Commands

A command that takes a `<job>` looks for it in the namespace you set.

| Command | What it does |
|---|---|
| `nops jobs` | List a page of the jobs with their [sync state](dashboard.md#sync-state-of-a-job). The [list flags](#lists) pick it. |
| `nops job <job>` | Show a job with its drift and deployments. |
| `nops deployments` | List a page of the deployments, newest first. The [list flags](#lists) pick it. |
| `nops deployment <id>` | Show a deployment with its diff, hook runs, and events. |
| `nops approve <id>` | Show the diff, ask for confirmation, and approve. `-yes` skips the question. |
| `nops reject <id>` | Reject. |
| `nops promote <id>` | Promote the canaries of a deployment that waits for it. |
| `nops retry <id>` | Retry a failed or rejected deployment and print the new ID. |
| `nops pause <job>` | [Pause](policies.md#pausing-a-job) a job, or every job with `-all`. `-reason` says why. |
| `nops resume <job>` | Resume a job, or every job with `-all`. |
| `nops deploy-now <job>` | Show the drift, ask for confirmation, and [deploy now](policies.md#sync-window). `-yes` skips the question. |
| `nops fetch` | Ask for a git poll now. |
| `nops backup <file>` | Save a [backup](running-nops.md#backups) of the database to the file. |
| `nops login` | Log in with the [login mode](dashboard.md#authentication) of Nops and save the session for its URL, in `nops/session` under your user configuration directory. Basic asks for the username and password. OIDC opens the browser, which must run on the same machine. The session lasts 12 hours. |
| `nops logout` | Revoke the session `nops login` saved and forget it. |
| `nops acl policy list` | List the [ACL policies](acl.md#acl-policies). |
| `nops acl policy info <name>` | Show an ACL policy with its rules. |
| `nops acl policy apply <name> <file>` | Create or replace an ACL policy from a file of rules. `-description` sets its description. |
| `nops acl policy delete <name>` | Delete an ACL policy. |
| `nops acl binding-rule list` | List the [binding rules](acl.md#binding-rules). |
| `nops acl binding-rule info <id>` | Show a binding rule. |
| `nops acl binding-rule create` | Create a binding rule. `-auth-method`, `-selector`, `-bind-type` (`policy` or `management`), `-bind-name`, and `-description` set it. |
| `nops acl binding-rule update <id>` | Replace a binding rule, with the same flags. |
| `nops acl binding-rule delete <id>` | Delete a binding rule. |
| `nops acl token list` | List the tokens. |
| `nops acl token info <accessor-id>` | Show a token. |
| `nops acl token create` | Create a token and print its secret once. `-name`, `-type` (`client` by default, or `management`), `-policy` (repeat it), and `-expires` set it. |
| `nops acl token delete <accessor-id>` | Revoke a token. Ask for confirmation first when it is the token the client uses. `-yes` skips the question. |
| `nops acl token delete-sessions <identity>` | Ask for confirmation and revoke every session of a person. `-yes` skips the question. |
| `nops acl token delete-created` | Ask for confirmation and revoke every token a person (`-creator-identity`) or a token (`-creator`, `bootstrap` included) created, and the tokens those created. `-yes` skips the question. |
| `nops acl token self` | Show the token the client uses. |
| `nops completion <shell>` | Print the completion script of `bash` or `zsh`. It completes the commands, their flags, and the files they take. Load it with `source <(nops completion bash)` in `~/.bashrc`, or with `source <(nops completion zsh)` after `compinit` in `~/.zshrc`. It runs offline. |
| `nops secret generate` | Print a new random secret for a file Nops reads, such as the [bootstrap token](acl.md#turning-it-on). It runs offline: it needs no `-addr` and no token. |

`approve` and `deploy-now` send the spec hash of the diff they showed, so a
newer diff is refused as on the dashboard.

## Lists

`nops jobs` and `nops deployments` take the parameters of the
[API's lists](api.md#lists) as flags. After a page that isn't the last, the
client prints the command for the next one.

| Flag | Parameter |
|---|---|
| `-per-page` | `per_page` |
| `-page-token` | `next_token` |
| `-search` | `q` |
| `-state` | `state` |
| `-namespace` | `namespace`. It doesn't read `NOPS_NAMESPACE`: without it, the list covers every namespace. |
| `-job` | `job`, on `nops deployments` |
| `-repository` | `repository` |
| `-policy` | `policy` |
| `-decided-by` | `decided_by`, on `nops deployments` |
| `-since`, `-until` | `since`, `until`, on `nops deployments` |
| `-warnings` | `warnings`, on `nops jobs` |

## Output

Text by default. `-json` prints the answer of the API, for a script. The diff
and the question go to the error output, so a pipe gets only the answer. A
command that fails exits with 1 and prints the message of the API.
