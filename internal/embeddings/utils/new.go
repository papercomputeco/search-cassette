// Package embeddingutils is the embeddings utility package
package embeddingutils

import (
	"fmt"

	"github.com/papercomputeco/search-cassette/internal/embeddings"
	"github.com/papercomputeco/search-cassette/internal/embeddings/ollama"
	"github.com/papercomputeco/search-cassette/internal/embeddings/openai"
)

type NewEmbedderOpts struct {
	ProviderType string
	TargetURL    string
	Model        string
	Dimensions   uint
	APIKey       string
}

func NewEmbedder(o *NewEmbedderOpts) (embeddings.Embedder, error) {
	switch o.ProviderType {
	case "ollama":
		return ollama.NewEmbedder(ollama.EmbedderConfig{
			BaseURL: o.TargetURL,
			Model:   o.Model,
		})
	case "openai":
		return openai.NewEmbedder(openai.EmbedderConfig{
			BaseURL:    o.TargetURL,
			Model:      o.Model,
			APIKey:     o.APIKey,
			Dimensions: o.Dimensions,
		})
	default:
		return nil, fmt.Errorf("unsupported embedding provider: %s", o.ProviderType)
	}
}
