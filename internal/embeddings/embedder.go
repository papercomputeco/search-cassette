// Package embeddings defines the embedding provider contract for the search
// cassette. It is the extracted form of tapes' pkg/embeddings: the Embedder
// interface, the structured APIError providers raise, and the ErrEmbedding
// sentinel every embedding failure unwraps to.
package embeddings

import (
	"context"
	"errors"
)

// ErrEmbedding is returned when embedding generation fails.
var ErrEmbedding = errors.New("embedding failed")

// Embedder provides text embedding capabilities.
type Embedder interface {
	// Embed converts text into a vector embedding.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Close releases any resources held by the embedder.
	Close() error
}
