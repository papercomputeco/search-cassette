package spanembed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvectorgo "github.com/pgvector/pgvector-go"
)

const (
	// DefaultTableName is the span-embedding table. It lives in the
	// same database as the versioned spans projection — candidate selection
	// and search both join against the current physical table family.
	DefaultTableName = "span_embeddings"

	// failuresTableSuffix names the sidecar table that records spans whose
	// embed failed deterministically (e.g. oversize that can't be chunked,
	// or a non-retryable API rejection). It is derived from the embedding
	// table name so a custom TableName keeps its failures alongside it.
	failuresTableSuffix = "_failures"

	// maxVectorDimensions is the upper bound pgvector supports for the
	// vector type.
	maxVectorDimensions = 16000

	// DefaultSpansTable and DefaultSpanTurnsTable are the tapes span
	// projection tables candidate selection and search join against.
	// They default to the current physical table family; when the
	// tapes_v1 contract views land, point the store at them instead
	// (e.g. "tapes_v1.spans") without a rebuild.
	DefaultSpansTable     = "spans_20260615"
	DefaultSpanTurnsTable = "span_turns_20260615"
)

// ErrNotInitialized is returned by reads when the embedding table does
// not exist yet — i.e. no embed pass has ever run against this store.
var ErrNotInitialized = errors.New("span embeddings not initialized: the search cassette's embed worker has not completed a pass yet")

// StoreConfig configures a span-embedding store.
type StoreConfig struct {
	// Schema is the cassette-owned Postgres schema the embedding tables
	// live in. Empty writes into the connection's default search path;
	// the cassette deployment sets it to the cassette name so the tables
	// land in the schema the manifest declares.
	Schema string

	// TableName defaults to DefaultTableName.
	TableName string

	// SpansTable and SpanTurnsTable name the tapes span projection
	// relations to read candidates and search context from. Either may
	// be schema-qualified ("tapes_v1.spans"). Empty takes the package
	// defaults.
	SpansTable     string
	SpanTurnsTable string

	// Dimensions is the embedding dimensionality. The table's vector
	// column is created with exactly this size and EnsureSchema
	// fail-fasts when an existing table disagrees — model and dims are
	// a deliberate, explicit pairing (e.g. text-embedding-3-large@1024
	// in cloud, embeddinggemma@768 on a local/dev deployment).
	Dimensions uint
}

// Store reads and writes span embeddings in the tapes Postgres
// database. The write path (EnsureSchema, Upsert, PruneOrphans)
// belongs to the embed pass; the read path (Search) backs the
// cassette's span search.
//
// Span identity is (trace_id, span_id): tapes dropped the organization
// concept (tapes#276), and while the projection tables may still carry a
// sentinel org_id column until the drop migration ships, nothing here
// reads or writes it — the store works identically before and after
// that column disappears.
type Store struct {
	pool      *pgxpool.Pool
	schema    string
	table     pgx.Identifier
	failures  pgx.Identifier
	spans     pgx.Identifier
	spanTurns pgx.Identifier
	dims      uint
	logger    *slog.Logger
}

// NewStore wraps an existing connection pool. It performs no IO:
// writers must call EnsureSchema before upserting, readers may query
// immediately and receive ErrNotInitialized until a writer has run.
func NewStore(pool *pgxpool.Pool, cfg StoreConfig, log *slog.Logger) (*Store, error) {
	if pool == nil {
		return nil, errors.New("span embedding store requires a connection pool")
	}
	if cfg.Dimensions == 0 {
		return nil, errors.New("span embedding dimensions cannot be 0, must be configured")
	}
	if cfg.Dimensions > maxVectorDimensions {
		return nil, fmt.Errorf("span embedding dimensions cannot exceed %d", maxVectorDimensions)
	}
	table := cfg.TableName
	if table == "" {
		table = DefaultTableName
	}
	return &Store{
		pool:      pool,
		schema:    cfg.Schema,
		table:     qualified(cfg.Schema, table),
		failures:  qualified(cfg.Schema, table+failuresTableSuffix),
		spans:     relationIdentifier(cfg.SpansTable, DefaultSpansTable),
		spanTurns: relationIdentifier(cfg.SpanTurnsTable, DefaultSpanTurnsTable),
		dims:      cfg.Dimensions,
		logger:    log,
	}, nil
}

// qualified builds the identifier for a cassette-owned table, inside the
// cassette schema when one is configured.
func qualified(schema, table string) pgx.Identifier {
	if schema == "" {
		return pgx.Identifier{table}
	}
	return pgx.Identifier{schema, table}
}

// relationIdentifier parses an optionally schema-qualified relation name
// ("spans_20260615" or "tapes_v1.spans") into an identifier, falling back to
// fallback when name is empty.
func relationIdentifier(name, fallback string) pgx.Identifier {
	if name == "" {
		name = fallback
	}
	if schema, table, ok := strings.Cut(name, "."); ok {
		return pgx.Identifier{schema, table}
	}
	return pgx.Identifier{name}
}

// baseName is the unqualified table name of a possibly schema-qualified
// identifier.
func baseName(id pgx.Identifier) string { return id[len(id)-1] }

// EnsureSchema creates the embedding table and its HNSW index sized to
// the configured dimensions, or fail-fasts when an existing table was
// created with different dimensions. pgvector cannot resize a column
// in place, so a dims change requires re-embedding into a new (or
// dropped) table — the error says so instead of letting every
// subsequent insert fail.
func (s *Store) EnsureSchema(ctx context.Context) error {
	table := s.table.Sanitize()

	// The pgvector extension must be installed by database
	// provisioning; runtime connections commonly cannot create
	// extensions in managed Postgres.
	var hasVector bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&hasVector); err != nil {
		return fmt.Errorf("checking vector extension: %w", err)
	}
	if !hasVector {
		return errors.New("vector extension is not installed in this database")
	}

	// The cassette owns its schema; the deployment grants the role CREATE
	// on the database (or pre-creates the schema) per the manifest.
	if s.schema != "" {
		createSchema := fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, pgx.Identifier{s.schema}.Sanitize())
		if _, err := s.pool.Exec(ctx, createSchema); err != nil {
			return fmt.Errorf("creating cassette schema: %w", err)
		}
	}

	var existingDims int32
	err := s.pool.QueryRow(ctx, `
		SELECT a.atttypmod
		FROM pg_attribute a
		WHERE a.attrelid = to_regclass($1)
		  AND a.attname = 'embedding'
	`, table).Scan(&existingDims)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Table does not exist yet; create it below.
	case err != nil:
		return fmt.Errorf("checking existing embedding dimensions: %w", err)
	case existingDims != int32(s.dims): // #nosec G115 -- NewStore bounds dimensions to maxVectorDimensions
		return fmt.Errorf(
			"existing table %s stores vector(%d) embeddings but %d dimensions are configured; re-embed into a new table or drop the old one",
			table, existingDims, s.dims,
		)
	}

	// No FK onto spans: the projection's down-migrations and prunes
	// must never be blocked by the embedding sidecar table. Orphans
	// are reaped by PruneOrphans instead. chunk_idx lets one span carry
	// several embedding rows: an oversized span is split into pieces, each
	// embedded and stored under its own chunk_idx (single-piece spans use
	// chunk_idx 0).
	createTable := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			trace_id     TEXT NOT NULL,
			span_id      TEXT NOT NULL,
			chunk_idx    INT NOT NULL DEFAULT 0,
			session_id   UUID,
			model        TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			embedded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			embedding    vector(%d) NOT NULL,
			PRIMARY KEY (trace_id, span_id, chunk_idx)
		)
	`, table, s.dims)
	if _, err := s.pool.Exec(ctx, createTable); err != nil {
		return fmt.Errorf("creating table: %w", err)
	}

	if err := s.ensureFailuresTable(ctx); err != nil {
		return err
	}

	indexName := pgx.Identifier{baseName(s.table) + "_embedding_idx"}.Sanitize()
	createIndex := fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS %s
		ON %s
		USING hnsw (embedding vector_cosine_ops)
	`, indexName, table)
	if _, err := s.pool.Exec(ctx, createIndex); err != nil {
		return fmt.Errorf("creating index: %w", err)
	}

	s.logger.Info("span embedding schema ready",
		"table", table,
		"dimensions", s.dims,
	)
	return nil
}

// ensureFailuresTable creates the sidecar that records deterministic embed
// failures. A row here means "this span's content cannot be embedded under
// this model, so don't keep retrying it every pass" — while preserving why,
// how big, and how often, so the loss is observable rather than silent. The
// span re-enters the embed set the moment its content (hash) or model changes.
func (s *Store) ensureFailuresTable(ctx context.Context) error {
	create := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			trace_id        TEXT NOT NULL,
			span_id         TEXT NOT NULL,
			session_id      UUID,
			model           TEXT NOT NULL,
			content_hash    TEXT NOT NULL,
			reason          TEXT NOT NULL,
			error_detail    TEXT,
			token_count     INT,
			attempts        INT NOT NULL DEFAULT 1,
			first_failed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			last_failed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (trace_id, span_id)
		)
	`, s.failures.Sanitize())
	if _, err := s.pool.Exec(ctx, create); err != nil {
		return fmt.Errorf("creating failures table: %w", err)
	}
	return nil
}

// ListCandidates pages every main llm span (keyset-ordered) joined with its
// current embedding state and any recorded failure. All of a span's chunk rows
// share one content hash and model, so the embedding join collapses them to a
// single row; the failure table is already one row per span. The caller decides
// per candidate whether an embed is due — content hashing happens in Go, so the
// query stays a cheap indexable join.
func (s *Store) ListCandidates(ctx context.Context, after Key, limit int) ([]Candidate, error) {
	table := s.table.Sanitize()
	query := fmt.Sprintf(`
		SELECT s.trace_id, s.span_id, s.session_id, s.input, s.output,
		       COALESCE(e.content_hash, ''), COALESCE(e.model, ''),
		       COALESCE(f.content_hash, ''), COALESCE(f.model, '')
		FROM `+s.spans.Sanitize()+` s
		LEFT JOIN (
			SELECT trace_id, span_id,
			       max(content_hash) AS content_hash, max(model) AS model
			FROM %s
			GROUP BY trace_id, span_id
		) e
		  ON e.trace_id = s.trace_id AND e.span_id = s.span_id
		LEFT JOIN %s f
		  ON f.trace_id = s.trace_id AND f.span_id = s.span_id
		WHERE s.kind = 'llm' AND s.call_kind = 'main'
		  AND (s.trace_id, s.span_id) > (@trace, @span)
		ORDER BY s.trace_id, s.span_id
		LIMIT @page
	`, table, s.failures.Sanitize())

	rows, err := s.pool.Query(ctx, query, pgx.NamedArgs{
		"trace": after.TraceID,
		"span":  after.SpanID,
		"page":  limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing embed candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var (
			c                   Candidate
			session             pgtype.UUID
			inp, outp           []byte
			hash, mdl           string
			failHash, failModel string
		)
		if err := rows.Scan(&c.TraceID, &c.SpanID, &session, &inp, &outp, &hash, &mdl, &failHash, &failModel); err != nil {
			return nil, fmt.Errorf("scanning embed candidate: %w", err)
		}
		if session.Valid {
			c.SessionID = uuid.UUID(session.Bytes).String()
		}
		c.Input = inp
		c.Output = outp
		c.ExistingHash = hash
		c.ExistingModel = mdl
		c.ExistingFailHash = failHash
		c.ExistingFailModel = failModel
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating embed candidates: %w", err)
	}
	return out, nil
}

// UpsertSpanChunks writes a span's embeddings as chunk rows 0..N-1 and removes
// any higher-indexed rows left over from a previous, longer chunking of the
// same span — all in one transaction so search never sees a partially-rewritten
// span. A successful write also clears any failure marker for the span: the
// content that once failed now embeds. Re-derives keep span identity stable, so
// re-embedding overwrites in place.
func (s *Store) UpsertSpanChunks(ctx context.Context, rec ChunkRecord) error {
	if len(rec.Embeddings) == 0 {
		return fmt.Errorf("upserting span embedding %s/%s: no embeddings", rec.TraceID, rec.SpanID)
	}
	session := pgtype.UUID{}
	if rec.SessionID != "" {
		parsed, err := uuid.Parse(rec.SessionID)
		if err != nil {
			return fmt.Errorf("parse session id: %w", err)
		}
		session = pgtype.UUID{Bytes: parsed, Valid: true}
	}

	table := s.table.Sanitize()
	insert := fmt.Sprintf(`
		INSERT INTO %s (trace_id, span_id, chunk_idx, session_id, model, content_hash, embedding, embedded_at)
		VALUES (@trace, @span, @chunk, @session, @model, @hash, @embedding, now())
		ON CONFLICT (trace_id, span_id, chunk_idx) DO UPDATE SET
			session_id   = EXCLUDED.session_id,
			model        = EXCLUDED.model,
			content_hash = EXCLUDED.content_hash,
			embedding    = EXCLUDED.embedding,
			embedded_at  = now()
	`, table)
	pruneTail := fmt.Sprintf(`DELETE FROM %s WHERE trace_id = @trace AND span_id = @span AND chunk_idx >= @count`, table)
	clearFailure := fmt.Sprintf(`DELETE FROM %s WHERE trace_id = @trace AND span_id = @span`, s.failures.Sanitize())

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin span embedding write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for i, embedding := range rec.Embeddings {
		if _, err := tx.Exec(ctx, insert, pgx.NamedArgs{
			"trace":     rec.TraceID,
			"span":      rec.SpanID,
			"chunk":     i,
			"session":   session,
			"model":     rec.Model,
			"hash":      rec.ContentHash,
			"embedding": pgvectorgo.NewVector(embedding),
		}); err != nil {
			return fmt.Errorf("upserting span embedding %s/%s chunk %d: %w", rec.TraceID, rec.SpanID, i, err)
		}
	}
	if _, err := tx.Exec(ctx, pruneTail, pgx.NamedArgs{
		"trace": rec.TraceID,
		"span":  rec.SpanID,
		"count": len(rec.Embeddings),
	}); err != nil {
		return fmt.Errorf("pruning stale chunks for %s/%s: %w", rec.TraceID, rec.SpanID, err)
	}
	if _, err := tx.Exec(ctx, clearFailure, pgx.NamedArgs{
		"trace": rec.TraceID,
		"span":  rec.SpanID,
	}); err != nil {
		return fmt.Errorf("clearing failure marker for %s/%s: %w", rec.TraceID, rec.SpanID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit span embedding write %s/%s: %w", rec.TraceID, rec.SpanID, err)
	}
	return nil
}

// RecordFailure marks a span as deterministically un-embeddable under its
// current content and model. The candidate gate skips such spans until their
// content (hash) or model changes; attempts accrue across passes so the table
// shows how persistent the failure is.
func (s *Store) RecordFailure(ctx context.Context, rec FailureRecord) error {
	session := pgtype.UUID{}
	if rec.SessionID != "" {
		parsed, err := uuid.Parse(rec.SessionID)
		if err != nil {
			return fmt.Errorf("parse session id: %w", err)
		}
		session = pgtype.UUID{Bytes: parsed, Valid: true}
	}
	tokenCount := pgtype.Int4{}
	if rec.TokenCount > 0 {
		tokenCount = pgtype.Int4{Int32: int32(rec.TokenCount), Valid: true} // #nosec G115 -- token counts are small positive ints
	}

	query := fmt.Sprintf(`
		INSERT INTO %s (trace_id, span_id, session_id, model, content_hash, reason, error_detail, token_count)
		VALUES (@trace, @span, @session, @model, @hash, @reason, @detail, @tokens)
		ON CONFLICT (trace_id, span_id) DO UPDATE SET
			session_id     = EXCLUDED.session_id,
			model          = EXCLUDED.model,
			content_hash   = EXCLUDED.content_hash,
			reason         = EXCLUDED.reason,
			error_detail   = EXCLUDED.error_detail,
			token_count    = EXCLUDED.token_count,
			attempts       = %s.attempts + 1,
			last_failed_at = now()
	`, s.failures.Sanitize(), s.failures.Sanitize())

	if _, err := s.pool.Exec(ctx, query, pgx.NamedArgs{
		"trace":   rec.TraceID,
		"span":    rec.SpanID,
		"session": session,
		"model":   rec.Model,
		"hash":    rec.ContentHash,
		"reason":  rec.Reason,
		"detail":  rec.ErrorDetail,
		"tokens":  tokenCount,
	}); err != nil {
		return fmt.Errorf("recording span embed failure %s/%s: %w", rec.TraceID, rec.SpanID, err)
	}
	return nil
}

// PruneOrphans removes embedding and failure rows whose span no longer exists
// as a main llm span — pruned by a re-derive, or reclassified out of the
// embeddable set. Returns the total number of rows removed across both tables.
func (s *Store) PruneOrphans(ctx context.Context) (int64, error) {
	orphanFilter := `
		WHERE NOT EXISTS (
			SELECT 1 FROM ` + s.spans.Sanitize() + ` s
			WHERE s.trace_id = e.trace_id AND s.span_id = e.span_id
			  AND s.kind = 'llm' AND s.call_kind = 'main'
		)`

	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s e%s`, s.table.Sanitize(), orphanFilter))
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil // nothing embedded yet, nothing to prune
		}
		return 0, fmt.Errorf("pruning orphaned span embeddings: %w", err)
	}
	pruned := tag.RowsAffected()

	failTag, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s e%s`, s.failures.Sanitize(), orphanFilter))
	if err != nil {
		if isUndefinedTable(err) {
			return pruned, nil
		}
		return pruned, fmt.Errorf("pruning orphaned span embed failures: %w", err)
	}
	return pruned + failTag.RowsAffected(), nil
}

// searchOverfetch multiplies topK when pulling nearest chunks, so that several
// chunks of the same span collapsing to one hit still leaves enough distinct
// spans to fill the result. Chunks per span are few, so a small factor suffices.
const searchOverfetch = 4

// Search returns the topK spans most similar to the query embedding, joined
// with their trace context (turn prompt and span payloads for the snippet).
// A span may have several chunk embeddings; results collapse to one hit per
// span, scored by the span's best-matching chunk.
func (s *Store) Search(ctx context.Context, embedding []float32, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 5
	}

	table := s.table.Sanitize()
	query := fmt.Sprintf(`
		WITH nearest AS (
			SELECT e.trace_id, e.span_id, e.session_id,
			       e.embedding <=> @embedding AS dist
			FROM %s e
			ORDER BY e.embedding <=> @embedding
			LIMIT @fetch
		),
		best AS (
			SELECT DISTINCT ON (trace_id, span_id) trace_id, span_id, session_id, dist
			FROM nearest
			ORDER BY trace_id, span_id, dist
		)
		SELECT b.trace_id, b.span_id, b.session_id,
		       1 - b.dist AS score,
		       t.user_prompt, s.model, s.started_at, s.input, s.output
		FROM best b
		JOIN `+s.spans.Sanitize()+` s
		  ON s.trace_id = b.trace_id AND s.span_id = b.span_id
		JOIN `+s.spanTurns.Sanitize()+` t
		  ON t.trace_id = b.trace_id
		ORDER BY b.dist
		LIMIT @topk
	`, table)

	rows, err := s.pool.Query(ctx, query, pgx.NamedArgs{
		"embedding": pgvectorgo.NewVector(embedding),
		"fetch":     topK * searchOverfetch,
		"topk":      topK,
	})
	if err != nil {
		if isUndefinedTable(err) {
			return nil, ErrNotInitialized
		}
		return nil, fmt.Errorf("querying span embeddings: %w", err)
	}
	defer rows.Close()

	var hits []Hit
	for rows.Next() {
		var (
			h         Hit
			session   pgtype.UUID
			startedAt pgtype.Timestamptz
			inp, outp []byte
		)
		if err := rows.Scan(&h.TraceID, &h.SpanID, &session, &h.Score, &h.UserPrompt, &h.Model, &startedAt, &inp, &outp); err != nil {
			return nil, fmt.Errorf("scanning span search hit: %w", err)
		}
		if session.Valid {
			h.SessionID = uuid.UUID(session.Bytes).String()
		}
		h.StartedAt = startedAt.Time
		h.Snippet = snippet(RenderSpanText(inp, outp))
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating span search hits: %w", err)
	}

	s.logger.Debug("span embedding search", "hits", len(hits))
	return hits, nil
}

// snippetRunes bounds the matched-span preview a search hit carries.
const snippetRunes = 280

// snippet truncates rendered span text for the search hit payload.
func snippet(text string) string {
	runes := []rune(text)
	if len(runes) <= snippetRunes {
		return text
	}
	return string(runes[:snippetRunes]) + "…"
}

// isUndefinedTable reports whether err is Postgres undefined_table —
// the read-before-any-embed-pass case.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
