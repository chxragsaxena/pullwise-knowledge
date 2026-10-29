// Package store keeps chunks and their embeddings in PostgreSQL with pgvector.
//
// Every operation takes a tenant and only ever touches that tenant's rows;
// tenant isolation lives here, in the WHERE clauses, so no caller can forget it.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// Open connects to PostgreSQL and verifies the connection.
func Open(ctx context.Context, databaseURL string) (*sql.DB, error) {
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
	return db, nil
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

// ReplaceRepo makes the stored chunks of a repo exactly the given ones, in one
// transaction: a failure leaves the previous index untouched.
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

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO chunks (tenant, repo, path, start_line, end_line, content, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7::vector)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, c := range chunks {
		if _, err := stmt.ExecContext(ctx, tenant, repo, c.Path, c.StartLine, c.EndLine, c.Content, vectorLiteral(c.Vector)); err != nil {
			return fmt.Errorf("insert chunk %s:%d: %w", c.Path, c.StartLine, err)
		}
	}
	return tx.Commit()
}

// Search returns the topK chunks of a repo closest to the query vector,
// skipping any in excludePaths (e.g. the files a caller already has in full).
func (s *Store) Search(ctx context.Context, tenant, repo string, query []float32, topK int, excludePaths []string) ([]Match, error) {
	if len(query) != embed.Dimension {
		return nil, fmt.Errorf("query vector has %d dimensions, want %d", len(query), embed.Dimension)
	}
	if excludePaths == nil {
		excludePaths = []string{}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, start_line, end_line, content, 1 - (embedding <=> $3::vector) AS score
		FROM chunks
		WHERE tenant = $1 AND repo = $2 AND NOT (path = ANY($5::text[]))
		ORDER BY embedding <=> $3::vector
		LIMIT $4`, tenant, repo, vectorLiteral(query), topK, excludePaths)
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

// vectorLiteral formats a vector in pgvector's text form, "[1,2,3]". Sending
// text and casting in SQL avoids needing a pgvector client library.
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
