-- Vector search needs the pgvector extension (the pgvector/pgvector image ships it).
CREATE EXTENSION IF NOT EXISTS vector;

-- One row per (tenant, repo): the commit currently indexed.
CREATE TABLE indexed_repos (
    tenant     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    ref        TEXT NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, repo)
);

-- Vector size must match embed.Dimension; no HNSW/IVFFlat index, as those are global across tenants.
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
