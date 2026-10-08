# API

The Nops JSON API: read jobs and deployments, and act on them from a script or
another tool.

Requests go under `/api/` and carry the token:

```sh
curl -H "Authorization: Bearer $NOPS_API_TOKEN" https://nops.example.com/api/jobs
```

From a terminal, the [command-line client](cli.md) does the same.

The API does what the token's [ACL policies](acl.md) allow, with the
dashboard's rules. Nops records the token as the
[actor](glossary.md#deployment-lifecycle). It works with both
[login modes](dashboard.md#authentication) and never contacts the identity
provider.

## Tokens

A management token creates tokens on the dashboard's *Administration* page, with
the API, or with [`nops acl token create`](cli.md#commands). A token expires
when you say, or never, and Nops deletes it when it does. Nops shows it once
and keeps only its hash.

A management token sees and revokes every token. A client token sees only
itself. A token stays valid until it expires or someone revokes it, even if
the person who created it can no longer log in. When you remove someone, revoke
their sessions and the tokens they created, each in one action. Revoking the
tokens a person or a token created also revokes the tokens those created.

## Endpoints

Requests with a body send JSON. Answers are JSON, except an action with
nothing to return: it answers `204` with no body, and `/api/fetch` answers
`202`.

| Request | What it does |
|---|---|
| `GET /api/jobs` | Every managed job with its [sync state](dashboard.md#sync-state-of-a-job), policy, and last deployment. |
| `GET /api/jobs/{namespace}/{job}` | One job with its drift, hold, and deployments. |
| `GET /api/deployments` | The active deployments and the latest finished ones, newest first. |
| `GET /api/deployments/{id}` | One deployment with its redacted diff, hook runs, and events. |
| `POST /api/deployments/{id}/approve` | Approve. Body: `{"spec_hash": "..."}`, the hash you read. |
| `POST /api/deployments/{id}/reject` | Reject. |
| `POST /api/deployments/{id}/promote` | Promote the canaries of a deployment that waits for it. |
| `POST /api/deployments/{id}/retry` | Retry a failed or rejected deployment. Answers `{"deployment_id": "..."}`. |
| `POST /api/jobs/{namespace}/{job}/pause` | [Pause](policies.md#pausing-a-job) a job. Optional body: `{"reason": "..."}`. |
| `POST /api/jobs/{namespace}/{job}/resume` | Resume a job. |
| `POST /api/jobs/{namespace}/{job}/deploy-now` | [Deploy now](policies.md#sync-window). Body: `{"spec_hash": "..."}`. Answers `{"deployment_id": "..."}`, empty when the next detection cycle starts it. |
| `POST /api/fetch` | Ask for a git poll now. |
| `GET /api/acl/policies` | The [ACL policies](acl.md#acl-policies): all with a management token, the token's own otherwise. |
| `GET /api/acl/policies/{name}` | One ACL policy with its rules. |
| `PUT /api/acl/policies/{name}` | Create or replace an ACL policy. Body: `{"description": "...", "rules": "..."}`. Management token only. |
| `DELETE /api/acl/policies/{name}` | Delete an ACL policy. Management token only. |
| `GET /api/acl/tokens` | Every token. Management token only. |
| `POST /api/acl/tokens` | Create a token. Body: `{"name": "...", "type": "client", "policies": ["..."], "expires_in": "720h"}`, where `type` defaults to `client` and an empty `expires_in` never expires. Answers `201` with the token and its secret, once. Management token only. |
| `GET /api/acl/tokens/{accessor_id}` | One token. Management token only. |
| `DELETE /api/acl/tokens/{accessor_id}` | Revoke a token. Management token only. |
| `POST /api/acl/tokens/revoke-sessions` | Revoke every session of a person. Body: `{"identity": "..."}`, the `identity` of one of their sessions. Answers `{"revoked": ["..."]}` with the accessor IDs. Management token only. |
| `POST /api/acl/tokens/revoke-created` | Revoke every token a person or a token created, and the tokens those created. Body: `{"creator_identity": "..."}` or `{"creator_accessor_id": "..."}`, which is `bootstrap` for the bootstrap token. Answers like `revoke-sessions`. Management token only. |
| `GET /api/acl/binding-rules` | Every [binding rule](acl.md#binding-rules). Management token only. |
| `POST /api/acl/binding-rules` | Create a binding rule. Body: `{"description": "...", "auth_method": "oidc", "selector": "...", "bind_type": "policy", "bind_name": "..."}`, where `bind_type` is `policy` or `management`. Answers `201` with the rule and its ID. Management token only. |
| `GET /api/acl/binding-rules/{id}` | One binding rule. Management token only. |
| `PUT /api/acl/binding-rules/{id}` | Replace a binding rule, with the same body. Management token only. |
| `DELETE /api/acl/binding-rules/{id}` | Delete a binding rule. Management token only. |
| `GET /api/acl/token/self` | The token of the request. |
| `GET /api/acl/changes` | The latest changes to ACL policies, binding rules, and tokens. Management token only. |
| `GET /api/openapi.json` | The [OpenAPI](https://spec.openapis.org/oas/v3.1.0) description of these endpoints. |

## Errors

An error answers `{"error": "..."}` with the status the dashboard would show:
`401` without a valid token, `403` for an action the token doesn't allow,
`404` for something that doesn't exist, `409` when the state moved since you
read it, and `400` for a bad request. An
approval with an old spec hash, or for a paused job, is a `409`.
