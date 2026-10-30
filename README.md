# pullwise-knowledge

The knowledge service for [PullWise](https://github.com/chxragsaxena/PullWise-Code-Reviewer-And-Mentor),
an AI pull-request reviewer. It indexes a repository's code and answers "what in
this repo is related to this?", so the reviewer can judge a change in the context
of the code around it instead of the diff alone.

It is a separate, independently deployable service with its own repository,
database, API contract and tests. PullWise calls it over HTTP; it never touches
PullWise's database.

```
PullWise worker ── signed token (+ optional mutual TLS) ──►  knowledge service ──► Cohere (embeddings)
                                                                     │
                                                                     ▼
                                                        PostgreSQL + pgvector (its own)
```

## How it works

1. **Index.** The caller sends a repository's source files and the commit they come from.
2. **Chunk.** Go files are cut at function and type boundaries (using `go/parser`),
   so a function is never split from its body; other languages are cut by lines,
   preferring blank lines, with a small overlap when a long block must be split.
3. **Embed.** Chunks become 1024-dimension vectors with Cohere's `embed-v5.0-pro`
   (queries use `embed-v5.0-fast`, which shares its embedding space).
4. **Store.** Chunks and vectors go into PostgreSQL with pgvector, in one
   transaction that replaces the repo's previous index.
5. **Search.** A query is embedded and matched by cosine similarity against that
   tenant's chunks of that repo. Alongside, a keyword search runs over the chunks' words
   (identifiers are split, so `CohereEmbedder` also matches "cohere" and "embedder", and file
   names count most). The two rankings are merged with reciprocal rank fusion. Both are
   filtered to the tenant and repo before ranking.

See [docs/api.md](docs/api.md) for the contract.

## Security model

| Layer | What it does |
|---|---|
| Private network | In Docker the database is not published at all; the service binds to localhost by default. |
| Signed tokens | Every call carries a short-lived HS256 JWT: issuer, audience and expiry are enforced, a lifetime over 5 minutes is refused, and only HS256 is accepted (not `none`, not other algorithms). |
| Tenant isolation | The token names the tenant; every query includes it. A tenant in a request body is an error. Tests prove one tenant can neither read, replace nor delete another's chunks. |
| Scopes | `index` and `search` are separate permissions. |
| Mutual TLS (optional) | With `TLS_CLIENT_CA_FILE` set, a caller without a certificate signed by your CA cannot even open a connection. Covered by tests for: no certificate, a certificate from another CA, and a client that wrongly trusts another server. |
| Hardening | Request size limits, unknown JSON fields rejected, strict input validation, timeouts, request IDs, and logs that never include tokens or code. Errors never expose internals. |

Known limits: the shared signing secret must be kept in both services' secret
stores and rotated by redeploying both; there is no per-caller rate limiting.
Chunks are sent to Cohere to be embedded, so code leaves the machine at that step.

## Design decisions

- **Exact search, not an approximate index.** HNSW and IVFFlat indexes are global:
  they find nearest neighbours across all tenants and filter afterwards, so with
  many tenants a search can silently return fewer results than exist. Searches here
  are always scoped to one tenant and repo (thousands of chunks at most), so an
  exact scan of that slice via a btree index is fast, complete, and cannot leak.
- **1024 dimensions.** Cohere's default (2048) is above pgvector's 2,000-dimension
  index limit; 1024 is half the storage and was verified to work with the API.
- **Code-aware chunking** keeps functions whole, which keeps retrieved context
  readable and useful for review.
- **Re-index only on change.** Indexing the same commit again is a no-op, which
  also protects the embedding provider's free-tier quota.

## Run it

```
cp .env.example .env       # POSTGRES_PASSWORD, COHERE_API_KEY, JWT_SECRET
docker compose up -d --build
curl http://127.0.0.1:8081/readyz
```

Mutual TLS: `go run ./cmd/devcerts -out certs`, then follow the comments in
`docker-compose.yml`.

## Test

```
go test ./...    # set TEST_DATABASE_URL to a pgvector database named *_test for the PostgreSQL tests
```

A throwaway database: `docker run -d -e POSTGRES_PASSWORD=... -e POSTGRES_DB=knowledge_test -p 127.0.0.1:55433:5432 pgvector/pgvector:pg18`.
