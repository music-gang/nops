# Access control

Who may do what in Nops. Every request to the dashboard or the
[API](api.md) carries a [token](glossary.md#access-control), and an
[ACL policy](#acl-policies) says what that token can do. A login ends in a
token too: [binding rules](#binding-rules) decide which one. The ACL is
modelled on [Nomad's](https://developer.hashicorp.com/nomad/docs/secure/acl).

## Turning it on

The ACL is off by default. Off, every token can do everything, and with OIDC
that includes everyone the identity provider lets in. Nops logs a WARN at
start and the *Administration* page says so.

To turn it on:

1. Generate the bootstrap secret into a file:
   `nops secret generate > bootstrap-token`.
2. Start Nops with `-acl` and `-acl-bootstrap-token-file`
   ([options](configuration.md#access-control)).
3. Paste the bootstrap token into the dashboard's login page, or use it from
   the [command line](cli.md) or the API. Create an ACL policy and a
   [binding rule](#binding-rules) for your login.
4. Log in. Until a binding rule matches you, a login fails.

A token works only in the mode it was made in: the tokens made while the ACL
is off stop working when you turn it on.

## Tokens

A token has an accessor ID, which is public and names the token in logs and
the audit, and a secret, which is the bearer. Nops shows the secret once, when
it creates the token, and keeps only its hash. There are two types:

- A **management** token can do everything, including administering ACL
  policies, binding rules, and tokens. Only a management token creates tokens.
- A **client** token carries ACL policies, and can read itself and the ACL
  policies it carries.

The **bootstrap token** is a management token read from
`-acl-bootstrap-token-file` at every start. To rotate it, change the file and
restart Nops. The tokens it created stay until someone revokes them.

## ACL policies

An ACL policy has a name and rules in HCL:

```hcl
namespace "prod" {
  capabilities = ["read", "approve"]
}

namespace "*" {
  policy = "read"
}

capabilities = ["fetch"]
```

A `namespace` block takes a name or a glob (`prod`, `pr*`, `*`). Its `policy`
is a shorthand for capabilities, and `capabilities` lists them. `policy =
"read"` is the same as `capabilities = ["read"]`; `write` and `deny` have no
capability of their own:

| `policy` | Capabilities |
|---|---|
| `read` | `read` |
| `write` | Every capability of a namespace. A capability added later joins it. |
| `deny` | None, and no other rule can grant one. |

| Capability | Allows |
|---|---|
| `read` | See the namespace's jobs and deployments. |
| `approve` | Approve and reject a deployment. |
| `promote` | Promote a deployment's canaries. |
| `retry` | Retry a failed or rejected deployment. |
| `deploy-now` | Deploy a job outside its sync window. |
| `pause` | Pause and resume a job. |

The top-level `capabilities` list holds the ones that belong to no namespace.
`fetch` asks for a git poll.

For a request on a namespace, Nops takes the most specific rule that matches
across all the token's ACL policies, so `prod` comes before `pr*`, and `pr*`
before `*`. If an ACL policy denies the namespace there, the request is
denied. Otherwise the capabilities of all the ACL policies add up. A token
sees only the namespaces it can `read`.

Nops looks up the ACL policies by name on every request, so editing one
changes every token that carries it. A name that no longer exists grants
nothing.

## Binding rules

A login only proves who you are. A binding rule says what that person gets:
it names an auth method (`oidc` or `basic`), a selector on the login's claims,
and what it binds: an ACL policy, or management. Nops adds up every rule that
matches, and a login that no rule matches fails. With the ACL off, rules are
not read and every login succeeds.

| Claim | Holds |
|---|---|
| `value.username` | The username, with either auth method. |
| `value.sub` | The `sub` of an OIDC login, which the provider never reassigns. |
| `list.groups` | The groups of an OIDC login, or of a local user in the groups file. |

The selector is a [go-bexpr](https://github.com/hashicorp/go-bexpr)
expression, as in Nomad's. An empty one matches every login of the auth
method.

```sh
nops acl binding-rule create -auth-method oidc \
  -selector '"ops" in list.groups' -bind-type policy -bind-name operator
```

Nops matches a person by the issuer and `sub` of an OIDC login, or by the
method and username of a basic one. The username is only what Nops shows: a
renamed account, or a new one that takes an old name, never inherits sessions
or the record of what a person created.

A login creates a **session token**. It carries the ACL policies the rules
bound, lasts 12 hours, and survives a restart. Logging out revokes it. A
changed rule applies from the next login. The dashboard shows your session
token's secret on the *Administration* page, to use it from the command line.

## Administration

The dashboard's *Administration* page, the [API](api.md#endpoints) and the
[command line](cli.md) manage ACL policies, binding rules, and tokens. With a management
token you also see the latest changes to them. A request without the needed
capability gets `403`.
