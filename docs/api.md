# API

The Nops JSON API: read jobs and deployments, and act on them from a script or
another tool.

Requests go under `/api/` and carry the token:

```sh
curl -H "Authorization: Bearer $NOPS_API_TOKEN" https://nops.example.com/api/jobs
```

From a terminal, the [command-line client](cli.md) does the same.

The API does what its owner can do on the dashboard, with the same rules. Nops
records the owner as the [actor](glossary.md#deployment-lifecycle). It works
with both [login modes](dashboard.md#authentication) and never contacts the
identity provider.

## Tokens

Create a token on the dashboard's *Tokens* page. A token belongs to the person
who created it, and expires in 7, 30, 90, or 365 days, or never. Nops shows it
once and keeps only its hash.

Every logged-in user sees every token and can revoke any of them. A token stays
valid until it expires or someone revokes it, even if its owner leaves the
allowlist or the users file: revoke the tokens of anyone you remove.

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
| `POST /api/jobs/{namespace}/{job}/deploy-now` | [Deploy now](policies.md#sync-window). Body: `{"spec_hash": "..."}`. |
| `POST /api/fetch` | Ask for a git poll now. |

## Errors

An error answers `{"error": "..."}` with the status the dashboard would show:
`401` without a valid token, `404` for something that doesn't exist, `409`
when the state moved since you read it, and `400` for a bad request. An
approval with an old spec hash, or for a paused job, is a `409`.
