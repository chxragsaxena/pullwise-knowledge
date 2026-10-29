// Package embed turns text into vectors for similarity search. Providers sit
// behind Embedder so the choice of service stays reversible.
package embed

import "context"

// Dimension is the length of every vector this service stores. It must match
// the vector(N) column in migrations/0001_init.sql. 1024 keeps vectors small
// and within pgvector's 2,000-dimension limit for indexes (Cohere's default
// output, 2048, is too large to index).
const Dimension = 1024

// Kind says what a text will be used for. Many embedding models produce
// different vectors for documents being indexed and queries searching them.
type Kind int

const (
	// Document is content being stored for later retrieval.
	Document Kind = iota
	// Query is a search string matched against stored documents.
	Query
)

// Embedder converts texts to Dimension-length vectors, one per input and in
// the same order.
type Embedder interface {
	Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error)
}
