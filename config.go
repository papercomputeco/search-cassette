package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaults for every environment variable this cassette reads. Only the
// database URL is required: a search cassette with nowhere to read spans from
// or write embeddings to cannot do anything at all.
const (
	defaultName   = "search"
	defaultListen = "0.0.0.0:9998"

	defaultEmbeddingProvider = "ollama"

	// Per-provider defaults, mirroring tapes' embedding config resolution:
	// model and dimensions are a deliberate, explicit pairing.
	defaultOllamaTarget     = "http://localhost:11434"
	defaultOllamaModel      = "embeddinggemma"
	defaultOllamaDimensions = 768

	defaultOpenAITarget     = "https://api.openai.com"
	defaultOpenAIModel      = "text-embedding-3-large"
	defaultOpenAIDimensions = 1024
)

// cassetteConfig is everything the deployment supplies through the
// environment. The CASSETTE_* names follow the manifest's config-key
// convention (llm.model -> CASSETTE_LLM_MODEL); TAPES_DATABASE_URL matches
// the credential name used across cassette deployments.
type cassetteConfig struct {
	Name   string
	Listen string

	DatabaseURL string
	WaitForDB   bool

	// Schema is the cassette-owned schema the embedding tables live in.
	// Defaults to the cassette name, matching the manifest's derived
	// grant plan. Set it to "" (CASSETTE_DB_SCHEMA="-") only when
	// pointing at a pre-cassette tapes deployment whose span_embeddings
	// table lives on the default search path.
	Schema string

	// SpansTable and SpanTurnsTable name the tapes span projection
	// relations to read. They default to the current physical table
	// family; when the tapes_v1 contract views land, point these at
	// them (e.g. "tapes_v1.spans") without a rebuild.
	SpansTable     string
	SpanTurnsTable string

	EmbeddingProvider   string
	EmbeddingTarget     string
	EmbeddingModel      string
	EmbeddingDimensions uint
	EmbeddingAPIKey     string

	EmbedInterval     time.Duration
	EmbedBatchSize    int
	EmbedMaxTextBytes int

	// OrgID optionally scopes the embed pass to one tenant.
	OrgID string
}

// schemaDisabled is the CASSETTE_DB_SCHEMA sentinel that opts out of a
// cassette-owned schema entirely (legacy default-search-path layout).
const schemaDisabled = "-"

// loadConfig reads the cassette's configuration from the environment.
func loadConfig() (cassetteConfig, error) {
	cfg := cassetteConfig{
		Name:        envOrDefault("CASSETTE_NAME", defaultName),
		Listen:      envOrDefault("CASSETTE_LISTEN", defaultListen),
		DatabaseURL: strings.TrimSpace(os.Getenv("TAPES_DATABASE_URL")),

		SpansTable:     strings.TrimSpace(os.Getenv("CASSETTE_SPANS_TABLE")),
		SpanTurnsTable: strings.TrimSpace(os.Getenv("CASSETTE_SPAN_TURNS_TABLE")),

		EmbeddingProvider: strings.ToLower(envOrDefault("CASSETTE_EMBEDDING_PROVIDER", defaultEmbeddingProvider)),
		EmbeddingTarget:   strings.TrimSpace(os.Getenv("CASSETTE_EMBEDDING_TARGET")),
		EmbeddingModel:    strings.TrimSpace(os.Getenv("CASSETTE_EMBEDDING_MODEL")),
		EmbeddingAPIKey:   strings.TrimSpace(os.Getenv("CASSETTE_EMBEDDING_API_KEY")),

		OrgID: strings.TrimSpace(os.Getenv("CASSETTE_ORG")),
	}

	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("TAPES_DATABASE_URL is required: the search cassette reads tapes' span projection and owns the span embedding tables")
	}

	cfg.Schema = envOrDefault("CASSETTE_DB_SCHEMA", cfg.Name)
	if cfg.Schema == schemaDisabled {
		cfg.Schema = ""
	}

	var err error
	if cfg.WaitForDB, err = envBool("CASSETTE_WAIT_FOR_DB", false); err != nil {
		return cfg, err
	}
	if cfg.EmbedInterval, err = envDuration("CASSETTE_EMBED_INTERVAL", 0); err != nil {
		return cfg, err
	}
	if cfg.EmbedBatchSize, err = envInt("CASSETTE_EMBED_BATCH_SIZE", 0); err != nil {
		return cfg, err
	}
	if cfg.EmbedMaxTextBytes, err = envInt("CASSETTE_EMBED_MAX_TEXT_BYTES", 0); err != nil {
		return cfg, err
	}
	dims, err := envInt("CASSETTE_EMBEDDING_DIMENSIONS", 0)
	if err != nil {
		return cfg, err
	}
	if dims < 0 {
		return cfg, fmt.Errorf("CASSETTE_EMBEDDING_DIMENSIONS must be positive")
	}
	cfg.EmbeddingDimensions = uint(dims)

	cfg.resolveEmbeddingDefaults()

	return cfg, nil
}

// resolveEmbeddingDefaults fills provider-specific defaults for whatever the
// deployment left unset — the environment-variable form of tapes'
// ResolveEmbeddingConfig: an omitted value takes the provider default, an
// explicit value always wins.
func (c *cassetteConfig) resolveEmbeddingDefaults() {
	switch c.EmbeddingProvider {
	case "openai":
		if c.EmbeddingTarget == "" {
			c.EmbeddingTarget = defaultOpenAITarget
		}
		if c.EmbeddingModel == "" {
			c.EmbeddingModel = defaultOpenAIModel
		}
		if c.EmbeddingDimensions == 0 {
			c.EmbeddingDimensions = defaultOpenAIDimensions
		}
	case "ollama":
		if c.EmbeddingTarget == "" {
			c.EmbeddingTarget = defaultOllamaTarget
		}
		if c.EmbeddingModel == "" {
			c.EmbeddingModel = defaultOllamaModel
		}
		if c.EmbeddingDimensions == 0 {
			c.EmbeddingDimensions = defaultOllamaDimensions
		}
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	return value, nil
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	return value, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	return value, nil
}
