-- Vector search needs the pgvector extension (the pgvector/pgvector image ships it).
CREATE EXTENSION IF NOT EXISTS vector;

-- One row per (tenant, repo): which commit is currently indexed. Every chunk
-- below belongs to exactly one of these, so deleting a repo deletes its chunks.
CREATE TABLE indexed_repos (
    tenant     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    ref        TEXT NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, repo)
);

-- The vector size must match embed.Dimension (1024).
--
-- There is deliberately no approximate (HNSW/IVFFlat) index here. Those indexes
-- are global: a query is answered from the nearest neighbours across ALL tenants
-- and only then filtered by tenant and repo, so with many tenants it can return
-- fewer results than exist for this one. Searches are always scoped to one
-- tenant+repo (a few thousand chunks at most), so scanning exactly that slice
-- via the btree index below is fast, complete and cannot leak across tenants.
CREATE TABLE chunks (
    id         BIGSERIAL PRIMARY KEY,
    tenant     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    path       TEXT NOT NULL,
    start_line INTEGER NOT NULL CHECK (start_line >= 1),
    end_line   INTEGER NOT NULL CHECK (end_line >= start_line),
    content    TEXT NOT NULL,
    embedding  vector(1024) NOT NULL,
    FOREIGN KEY (tenant, repo) REFERENCES indexed_repos (tenant, repo) ON DELETE CASCADE
);
CREATE INDEX chunks_tenant_repo_idx ON chunks (tenant, repo);
