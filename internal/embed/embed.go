// Package embed turns text into vectors for similarity search.
package embed

import "context"

// Dimension is the vector length; it must match vector(N) in migrations/0001_init.sql.
const Dimension = 1024

// Kind says whether a text is a document or a query (models embed them differently).
type Kind int

const (
	// Document is content being stored for later retrieval.
	Document Kind = iota
	// Query is a search string matched against stored documents.
	Query
)

// Embedder converts texts to Dimension-length vectors, one per input, in order.
type Embedder interface {
	Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error)
}
