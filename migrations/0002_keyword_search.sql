-- Keyword search next to vector search; the words are split from identifiers in Go (chunk.SearchText).
-- Existing indexes are only a cache of GitHub, so they are cleared and rebuilt on the next request.
DELETE FROM indexed_repos;

ALTER TABLE chunks
    ADD COLUMN path_words    TEXT NOT NULL DEFAULT '',
    ADD COLUMN content_words TEXT NOT NULL DEFAULT '',
    ADD COLUMN search tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', path_words), 'A') ||
        setweight(to_tsvector('simple', content_words), 'B')
    ) STORED;

CREATE INDEX chunks_search_idx ON chunks USING gin (search);
