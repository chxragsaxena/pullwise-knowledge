// Package store keeps chunks and embeddings in PostgreSQL with pgvector, scoped per tenant.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/chxragsaxena/pullwise-knowledge/internal/chunk"
	"github.com/chxragsaxena/pullwise-knowledge/internal/embed"
)

// Store is the PostgreSQL implementation of chunk storage and search.
type Store struct{ db *sql.DB }

// New wraps an open database handle.
func New(db *sql.DB) *Store { return &Store{db: db} }

var schemaPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// withSearchPath returns the URL with the connection's default schema set to schema. public stays on the
// path so the pgvector type is found even if the extension was installed there.
func withSearchPath(databaseURL, schema string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("DATABASE_SCHEMA needs DATABASE_URL in the postgres:// form")
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// createSchema makes sure schema exists, using its own short connection.
func createSchema(ctx context.Context, databaseURL, schema string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS "`+schema+`"`); err != nil {
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	return nil
}

// Open connects to PostgreSQL and verifies the connection. A non-empty schema keeps all tables in
// that schema (created if missing), so the service can share a database with another application.
func Open(ctx context.Context, databaseURL, schema string) (*sql.DB, error) {
	if schema != "" {
		if !schemaPattern.MatchString(schema) {
			return nil, fmt.Errorf("DATABASE_SCHEMA %q must be lowercase letters, digits and underscores", schema)
		}
		scoped, err := withSearchPath(databaseURL, schema)
		if err != nil {
			return nil, err
		}
		if err := createSchema(ctx, databaseURL, schema); err != nil {
			return nil, err
		}
		databaseURL = scoped
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if schema != "" {
		if err := checkSchema(pingCtx, db, schema); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// checkSchema fails if the connection ignored the requested schema, as a pooled connection does.
// Carrying on would put this service's tables in the public schema, among another application's.
func checkSchema(ctx context.Context, db *sql.DB, want string) error {
	var got string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&got); err != nil {
		return fmt.Errorf("check the database schema: %w", err)
	}
	if got != want {
		return fmt.Errorf("the database connection uses schema %q, not %q: DATABASE_URL is probably a pooled connection (host contains \"-pooler\"); use the direct connection string", got, want)
	}
	return nil
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Stored is a chunk together with its vector.
type Stored struct {
	chunk.Chunk
	Vector []float32
}

// Match is one search result.
type Match struct {
	Path      string
	StartLine int
	EndLine   int
	Content   string
	// Score is cosine similarity: 1 is identical, 0 unrelated.
	Score float64
}

// IndexedRef returns the commit currently indexed for a repo, if any.
func (s *Store) IndexedRef(ctx context.Context, tenant, repo string) (string, bool, error) {
	var ref string
	err := s.db.QueryRowContext(ctx,
		"SELECT ref FROM indexed_repos WHERE tenant = $1 AND repo = $2", tenant, repo).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read indexed ref: %w", err)
	}
	return ref, true, nil
}

// ReplaceRepo atomically replaces the stored chunks of a repo.
func (s *Store) ReplaceRepo(ctx context.Context, tenant, repo, ref string, chunks []Stored) error {
	for i, c := range chunks {
		if len(c.Vector) != embed.Dimension {
			return fmt.Errorf("chunk %d has a %d-dimension vector, want %d", i, len(c.Vector), embed.Dimension)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Deleting the repo row cascades to its chunks.
	if _, err := tx.ExecContext(ctx, "DELETE FROM indexed_repos WHERE tenant = $1 AND repo = $2", tenant, repo); err != nil {
		return fmt.Errorf("clear previous index: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO indexed_repos (tenant, repo, ref) VALUES ($1, $2, $3)", tenant, repo, ref); err != nil {
		return fmt.Errorf("record index: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO chunks (tenant, repo, path, start_line, end_line, content, embedding, path_words, content_words)
		VALUES ($1, $2, $3, $4, $5, $6, $7::vector, $8, $9)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, c := range chunks {
		if _, err := stmt.ExecContext(ctx, tenant, repo, c.Path, c.StartLine, c.EndLine, c.Content, vectorLiteral(c.Vector),
			chunk.SearchText(c.Path), chunk.SearchText(c.Content)); err != nil {
			return fmt.Errorf("insert chunk %s:%d: %w", c.Path, c.StartLine, err)
		}
	}
	return tx.Commit()
}

// minCandidates is the least results taken from each of the two searches before they are fused.
const minCandidates = 30

// vectorOnlySQL ranks by cosine distance alone.
const vectorOnlySQL = `
	SELECT path, start_line, end_line, content, 1 - (embedding <=> $3::vector) AS score
	FROM chunks
	WHERE tenant = $1 AND repo = $2 AND NOT (path = ANY($5::text[]))
	ORDER BY embedding <=> $3::vector
	LIMIT $4`

// hybridSQL fuses the vector ranking and the keyword ranking with reciprocal rank fusion.
// Both lists are filtered to the tenant and repo before ranking; score stays the cosine similarity.
// The 60 in the fusion is the usual reciprocal-rank constant.
const hybridSQL = `
	WITH vec AS (
		SELECT id, row_number() OVER (ORDER BY embedding <=> $3::vector) AS r
		FROM chunks
		WHERE tenant = $1 AND repo = $2 AND NOT (path = ANY($5::text[]))
		ORDER BY embedding <=> $3::vector
		LIMIT $6),
	txt AS (
		SELECT id, row_number() OVER (ORDER BY ts_rank_cd(search, q) DESC, id) AS r
		FROM chunks, to_tsquery('simple', $7) q
		WHERE tenant = $1 AND repo = $2 AND NOT (path = ANY($5::text[])) AND search @@ q
		ORDER BY ts_rank_cd(search, q) DESC, id
		LIMIT $6),
	fused AS (
		SELECT id, sum(1.0 / (60 + r)) AS rrf
		FROM (SELECT id, r FROM vec UNION ALL SELECT id, r FROM txt) u
		GROUP BY id)
	SELECT c.path, c.start_line, c.end_line, c.content, 1 - (c.embedding <=> $3::vector) AS score
	FROM fused f JOIN chunks c ON c.id = f.id
	ORDER BY f.rrf DESC, c.id
	LIMIT $4`

// Search returns the topK chunks of a repo that best match the query vector and the keywords,
// skipping excludePaths. Without usable keywords it ranks by vector similarity alone.
func (s *Store) Search(ctx context.Context, tenant, repo string, query []float32, keywords string, topK int, excludePaths []string) ([]Match, error) {
	if len(query) != embed.Dimension {
		return nil, fmt.Errorf("query vector has %d dimensions, want %d", len(query), embed.Dimension)
	}
	if excludePaths == nil {
		excludePaths = []string{}
	}

	var rows *sql.Rows
	var err error
	if terms := chunk.QueryTerms(keywords); len(terms) > 0 {
		candidates := max(minCandidates, topK*5)
		rows, err = s.db.QueryContext(ctx, hybridSQL, tenant, repo, vectorLiteral(query), topK, excludePaths,
			candidates, strings.Join(terms, " | "))
	} else {
		rows, err = s.db.QueryContext(ctx, vectorOnlySQL, tenant, repo, vectorLiteral(query), topK, excludePaths)
	}
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var out []Match
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.Path, &m.StartLine, &m.EndLine, &m.Content, &m.Score); err != nil {
			return nil, fmt.Errorf("scan match: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteRepo removes a repo's index. It reports whether anything was deleted.
func (s *Store) DeleteRepo(ctx context.Context, tenant, repo string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM indexed_repos WHERE tenant = $1 AND repo = $2", tenant, repo)
	if err != nil {
		return false, fmt.Errorf("delete repo: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// vectorLiteral formats a vector in pgvector's text form, "[1,2,3]".
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
