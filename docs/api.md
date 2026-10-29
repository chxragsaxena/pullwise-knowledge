# Knowledge service API (v1)

JSON over HTTP. Every `/v1` route needs `Authorization: Bearer <token>` (see
[Authentication](#authentication)). Errors always look like:

```json
{ "error": { "code": "invalid_request", "message": "..." } }
```

Send an `X-Request-ID` header (letters, digits, `.`, `_`, `-`, up to 64 characters)
to follow one request across services in the logs; it is echoed back, or generated
if missing.

## Authentication

A short-lived JWT, signed HS256 with a secret shared by the caller and this service.

| Claim | Value |
|---|---|
| `iss` | the configured issuer (default `pullwise`) |
| `aud` | the configured audience (default `pullwise-knowledge`) |
| `iat`, `exp` | required; `exp - iat` must be at most 5 minutes (callers should use about a minute) |
| `tenant` | whose data this call may touch: letters, digits, `_`, `-`, up to 64 characters |
| `scopes` | any of `"index"`, `"search"` |

The tenant always comes from the token. A `tenant` field in a request body is
rejected with `400`, not ignored. Missing or invalid token: `401`. Valid token
without the needed scope: `403`.

## `POST /v1/index`  (scope `index`)

Replaces the tenant's index of one repository.

```json
{ "repo": "owner/name", "ref": "<commit sha>", "files": [ { "path": "a.go", "content": "..." } ] }
```

- `repo`: `owner/name`. `ref`: up to 100 characters from `A-Za-z0-9._/-`.
- 1 to 1,000 files, each up to 200,000 bytes; at most 3,000 chunks in total; body
  up to 16 MiB (`413 too_large` beyond these).
- If the tenant already has this repo indexed at this `ref`, nothing is done and no
  embeddings are requested.

`200`: `{ "repo", "ref", "status": "indexed" | "unchanged", "files", "chunks" }`.
The replacement is atomic: a failure leaves the previous index untouched.
`502 embedding_unavailable` if the embedding provider fails.

## `POST /v1/search`  (scope `search`)

```json
{ "repo": "owner/name", "query": "how do we retry a failing call?", "topK": 5, "excludePaths": ["a.go"] }
```

- `query`: 1 to 4,000 characters. `topK`: 1 to 20 (default 5). `excludePaths`: up to
  100 paths to leave out, e.g. files the caller already has in full.

`200`: `{ "results": [ { "path", "startLine", "endLine", "content", "score" } ] }`,
best first. `score` is cosine similarity (1 identical, 0 unrelated). A repo that is
not indexed gives an empty list.

## `GET /v1/repos/{owner}/{name}`  (scope `search`)

`200`: `{ "repo", "ref" }`, the commit currently indexed for the token's tenant, or
`404` if none. Lets a caller skip downloading and re-sending a repository that is
already up to date.

## `DELETE /v1/repos/{owner}/{name}`  (scope `index`)

`204` if the tenant's index was deleted, `404` if there was none.

## `GET /healthz`, `GET /readyz`

Unauthenticated. `/readyz` is `503` when the database is unreachable (the reason is
not exposed).
