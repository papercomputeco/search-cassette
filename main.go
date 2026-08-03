// Command search-cassette is the tapes span-search cassette.
//
// It is the extracted form of tapes' search surface: the semantic span
// search endpoint (tapes' GET /v1/search/spans) and the embed worker that
// keeps the span-embedding projection current (tapes' `tapes serve
// embed-worker`), packaged as one independently deployed HTTP service that
// tapes admits and reverse-proxies under /v1/cassettes/search.
//
// Three things make it a cassette:
//
//  1. /ping answers 200, which is the api.health anchor core probes.
//  2. /openapi serves its OpenAPI document with the cassette manifest
//     embedded at x-tapes-cassette, which core fetches and aggregates.
//  3. /api/search/spans is its actual API, served under the prefix its
//     manifest declares. Core strips that head and republishes the path
//     under /v1/cassettes/search.
//
// It reads tapes' span projection (the depends.views declaration), owns the
// span_embeddings tables in its own schema (the tables declaration), and
// runs the embed pass on its own interval in-process — search and embedding
// share one failure domain here because both ARE the search feature; tapes'
// derivation is a separate process either way and stays untouched whatever
// this cassette does.
//
// Configuration arrives entirely through the environment supplied by the
// deployment. The cassette reads env vars and nothing else — no config file,
// no flags, and no knowledge of which runtime started it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	embeddingutils "github.com/papercomputeco/search-cassette/internal/embeddings/utils"
	"github.com/papercomputeco/search-cassette/internal/embedworker"
	"github.com/papercomputeco/search-cassette/internal/spanembed"
)

// Startup connection bounds mirror the tapes embed worker: a small pool with
// a bounded connect timeout beats pgx's NumCPU-based default for a
// single-loop worker plus a low-QPS search endpoint.
const (
	connectTimeout    = 10 * time.Second
	maxPoolConns      = 4
	maxConnectBackoff = 30 * time.Second

	shutdownTimeout = 5 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(logger); err != nil {
		logger.Error("search cassette failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	pool, err := connect(ctx, cfg, logger)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if err != nil {
		return err
	}
	defer pool.Close()

	embedder, err := embeddingutils.NewEmbedder(&embeddingutils.NewEmbedderOpts{
		ProviderType: cfg.EmbeddingProvider,
		TargetURL:    cfg.EmbeddingTarget,
		Model:        cfg.EmbeddingModel,
		Dimensions:   cfg.EmbeddingDimensions,
		APIKey:       cfg.EmbeddingAPIKey,
	})
	if err != nil {
		return fmt.Errorf("could not create embedder: %w", err)
	}
	defer func() { _ = embedder.Close() }()

	// Schema is ensured at startup so a model/dimensions misconfiguration
	// fails the process immediately and visibly.
	store, err := spanembed.NewStore(pool, spanembed.StoreConfig{
		Schema:         cfg.Schema,
		Dimensions:     cfg.EmbeddingDimensions,
		OrgID:          cfg.OrgID,
		SpansTable:     cfg.SpansTable,
		SpanTurnsTable: cfg.SpanTurnsTable,
	}, logger)
	if err != nil {
		return fmt.Errorf("could not create span embedding store: %w", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("span embedding schema: %w", err)
	}

	pass, err := spanembed.NewPass(store, store, embedder, spanembed.PassConfig{
		Model:        cfg.EmbeddingModel,
		Dimensions:   cfg.EmbeddingDimensions,
		BatchSize:    cfg.EmbedBatchSize,
		MaxTextBytes: cfg.EmbedMaxTextBytes,
	}, logger)
	if err != nil {
		return fmt.Errorf("could not create span embed pass: %w", err)
	}

	logger.Info("span embedding enabled",
		"embedding_provider", cfg.EmbeddingProvider,
		"embedding_target", cfg.EmbeddingTarget,
		"embedding_model", cfg.EmbeddingModel,
		"embedding_dimensions", cfg.EmbeddingDimensions,
		"batch_size", cfg.EmbedBatchSize,
		"max_text_bytes", cfg.EmbedMaxTextBytes,
	)

	worker := embedworker.NewWorker(embedworker.Config{
		Interval: cfg.EmbedInterval,
		Ready: func(ctx context.Context) error {
			return pool.Ping(ctx)
		},
	}, pass, logger)

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: (&server{
			name:     cfg.Name,
			embedder: embedder,
			searcher: store,
			ready:    worker.Ready,
			metrics:  worker.Metrics().Handler(),
			logger:   logger,
		}).routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The worker loop and the listener share the signal context: either
	// failing terminally, or the process manager asking to stop, drains both.
	errs := make(chan error, 2)
	go func() {
		errs <- worker.Run(ctx)
	}()
	go func() {
		logger.Info("search cassette listening",
			"listen", cfg.Listen, "name", cfg.Name, "schema", cfg.Schema)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		if err != nil {
			return err
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

// connect opens the Postgres pool. By default an unreachable database is a
// startup error; with CASSETTE_WAIT_FOR_DB the cassette retries with
// exponential backoff until the database appears or ctx is canceled.
func connect(ctx context.Context, cfg cassetteConfig, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres DSN: %w", err)
	}
	poolCfg.ConnConfig.ConnectTimeout = connectTimeout
	poolCfg.MaxConns = maxPoolConns

	open := func() (*pgxpool.Pool, error) {
		pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			return nil, err
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, err
		}
		return pool, nil
	}

	pool, err := open()
	if err == nil {
		return pool, nil
	}
	if !cfg.WaitForDB {
		return nil, fmt.Errorf("postgres unreachable at startup (set CASSETTE_WAIT_FOR_DB=true to retry instead): %w", err)
	}

	backoff := time.Second
	for attempt := 1; ; attempt++ {
		logger.Warn("postgres unreachable, retrying",
			"attempt", attempt,
			"retry_in", backoff,
			"error", err.Error(),
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		pool, err = open()
		if err == nil {
			logger.Info("postgres reachable", "attempts", attempt+1)
			return pool, nil
		}
		backoff = min(backoff*2, maxConnectBackoff)
	}
}
