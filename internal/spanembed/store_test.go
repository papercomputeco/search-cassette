package spanembed_test

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/papercomputeco/search-cassette/internal/spanembed"
)

// Store integration specs run against the Postgres service the Dagger Test
// check binds as TEST_POSTGRES_DSN. Without it (a plain `go test ./...`)
// they skip: provisioning Postgres is the check's job, not the suite's.
//
// The spans relation is a test-owned stand-in holding exactly the columns
// candidate selection reads — SpansTable exists as config precisely so a
// test fixture can take the place of the tapes_v1 view.
var _ = Describe("Store", func() {
	const (
		spansTable = "spans_store_test"
		embTable   = "span_embeddings_store_test"
	)

	var (
		ctx   context.Context
		pool  *pgxpool.Pool
		store *spanembed.Store
	)

	BeforeEach(func() {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			Skip("TEST_POSTGRES_DSN is not set; run `make check` so Dagger provisions Postgres")
		}

		ctx = context.Background()
		var err error
		pool, err = pgxpool.New(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())

		// EnsureSchema requires pgvector to be installed already — a
		// deployment concern, so here the test is the deployment.
		_, err = pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
		Expect(err).NotTo(HaveOccurred())

		_, err = pool.Exec(ctx, `
			CREATE TABLE `+spansTable+` (
				trace_id   TEXT NOT NULL,
				span_id    TEXT NOT NULL,
				session_id UUID,
				kind       TEXT NOT NULL,
				call_kind  TEXT NOT NULL,
				input      JSONB,
				output     JSONB,
				PRIMARY KEY (trace_id, span_id)
			)
		`)
		Expect(err).NotTo(HaveOccurred())

		store, err = spanembed.NewStore(pool, spanembed.StoreConfig{
			TableName:  embTable,
			SpansTable: spansTable,
			Dimensions: 3,
		}, slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		Expect(store.EnsureSchema(ctx)).To(Succeed())
	})

	AfterEach(func() {
		if pool == nil {
			return
		}
		for _, table := range []string{spansTable, embTable, embTable + "_failures"} {
			_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+table)
			Expect(err).NotTo(HaveOccurred())
		}
		pool.Close()
		pool = nil
	})

	insertSpan := func(spanID, kind, callKind, input string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO `+spansTable+` (trace_id, span_id, kind, call_kind, input)
			VALUES ('trace_a', $1, $2, $3, $4::jsonb)
		`, spanID, kind, callKind, input)
		Expect(err).NotTo(HaveOccurred())
	}

	It("lists a chunked span as one candidate and surfaces failure state, across pages", func() {
		insertSpan("llm_a", "llm", "main", `[{"type":"text","text":"alpha content"}]`)
		insertSpan("llm_b", "llm", "main", `[{"type":"text","text":"beta content"}]`)
		insertSpan("tool_c", "tool", "main", `[]`)

		// llm_a: three chunk rows sharing one hash+model must collapse to
		// a single candidate. llm_b: a recorded failure must surface on
		// its candidate. tool_c: not an llm span, never a candidate.
		Expect(store.UpsertSpanChunks(ctx, spanembed.ChunkRecord{
			TraceID: "trace_a", SpanID: "llm_a", Model: "m", ContentHash: "ha",
			Embeddings: [][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}},
		})).To(Succeed())
		Expect(store.RecordFailure(ctx, spanembed.FailureRecord{
			TraceID: "trace_a", SpanID: "llm_b", Model: "m",
			ContentHash: "hb", Reason: "oversize",
		})).To(Succeed())

		candidates, err := store.ListCandidates(ctx, spanembed.Key{}, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(2))
		Expect(candidates[0].SpanID).To(Equal("llm_a"))
		Expect(candidates[0].ExistingHash).To(Equal("ha"))
		Expect(candidates[0].ExistingModel).To(Equal("m"))
		Expect(candidates[0].ExistingFailHash).To(BeEmpty())
		Expect(candidates[1].SpanID).To(Equal("llm_b"))
		Expect(candidates[1].ExistingHash).To(BeEmpty())
		Expect(candidates[1].ExistingFailHash).To(Equal("hb"))
		Expect(candidates[1].ExistingFailModel).To(Equal("m"))

		// Keyset pagination: a page of one, resumed from its key, yields
		// exactly the remaining span, then an empty page.
		page, err := store.ListCandidates(ctx, spanembed.Key{}, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(page).To(HaveLen(1))
		Expect(page[0].SpanID).To(Equal("llm_a"))
		page, err = store.ListCandidates(ctx, page[0].Key(), 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(page).To(HaveLen(1))
		Expect(page[0].SpanID).To(Equal("llm_b"))
		page, err = store.ListCandidates(ctx, page[0].Key(), 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(page).To(BeEmpty())
	})
})
