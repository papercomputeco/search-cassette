package spanembed_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/papercomputeco/search-cassette/internal/spanembed"
)

// Store.Search specs run against stand-ins for the three tapes_v1 views a
// search joins, holding exactly the columns it reads — every one of them a
// column the published tapes_v1 views carry.
//
// The pool disables sequential scans and explicit sorts so the planner
// drives the search from the HNSW index's ordered scan even on fixture-sized
// tables. Without that, a few hundred rows plan as an exact scan-and-sort,
// and the iterative-scan spec would pass whether or not the scan were
// iterative (verified: with hnsw.iterative_scan off it fails).
var _ = Describe("Store.Search", func() {
	const (
		spansTable    = "spans_search_test"
		turnsTable    = "span_turns_search_test"
		sessionsTable = "sessions_search_test"
		embTable      = "span_embeddings_search_test"

		sessionAda = "00000000-0000-4000-8000-00000000000a"
		sessionBob = "00000000-0000-4000-8000-00000000000b"
		// sessionGone is referenced by a span but has no sessions row.
		sessionGone = "00000000-0000-4000-8000-0000000000ff"
		// staleSession is what the embedding row recorded at embed time;
		// the span has since moved to its current session.
		staleSession = "00000000-0000-4000-8000-000000000999"
	)

	var (
		ctx   context.Context
		pool  *pgxpool.Pool
		store *spanembed.Store
		query = []float32{1, 0, 0}
	)

	BeforeEach(func() {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			Skip("TEST_POSTGRES_DSN is not set; run `make check` so Dagger provisions Postgres")
		}

		ctx = context.Background()
		cfg, err := pgxpool.ParseConfig(dsn)
		Expect(err).NotTo(HaveOccurred())
		cfg.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
		cfg.ConnConfig.RuntimeParams["enable_sort"] = "off"
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())

		_, err = pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
		Expect(err).NotTo(HaveOccurred())

		for _, ddl := range []string{
			`CREATE TABLE ` + spansTable + ` (
				trace_id   TEXT NOT NULL,
				span_id    TEXT NOT NULL,
				session_id UUID,
				kind       TEXT NOT NULL DEFAULT 'llm',
				call_kind  TEXT NOT NULL DEFAULT 'main',
				model      TEXT,
				started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				input      JSONB,
				output     JSONB,
				PRIMARY KEY (trace_id, span_id)
			)`,
			`CREATE TABLE ` + turnsTable + ` (
				trace_id    TEXT PRIMARY KEY,
				session_id  UUID,
				user_prompt TEXT NOT NULL DEFAULT '',
				started_at  TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE ` + sessionsTable + ` (
				id                 UUID PRIMARY KEY,
				auth_subject       TEXT NOT NULL DEFAULT '',
				harness_id         TEXT NOT NULL DEFAULT '',
				cwd                TEXT NOT NULL DEFAULT '',
				started_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
				last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
				ended_at           TIMESTAMPTZ,
				derived_title      TEXT,
				display_name       TEXT
			)`,
		} {
			_, err = pool.Exec(ctx, ddl)
			Expect(err).NotTo(HaveOccurred())
		}

		store, err = spanembed.NewStore(pool, spanembed.StoreConfig{
			TableName:      embTable,
			SpansTable:     spansTable,
			SpanTurnsTable: turnsTable,
			SessionsTable:  sessionsTable,
			Dimensions:     3,
		}, slog.New(slog.DiscardHandler))
		Expect(err).NotTo(HaveOccurred())
		Expect(store.EnsureSchema(ctx)).To(Succeed())
	})

	AfterEach(func() {
		if pool == nil {
			return
		}
		for _, table := range []string{spansTable, turnsTable, sessionsTable, embTable, embTable + "_failures"} {
			_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+table)
			Expect(err).NotTo(HaveOccurred())
		}
		pool.Close()
		pool = nil
	})

	insertSession := func(id, subject, cwd, harness string, lastSeen time.Time, ended *time.Time, displayName, derivedTitle string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO `+sessionsTable+`
				(id, auth_subject, cwd, harness_id, last_seen_at, ended_at, display_name, derived_title)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, id, subject, cwd, harness, lastSeen, ended, displayName, derivedTitle)
		Expect(err).NotTo(HaveOccurred())
	}

	// insertSpan writes one main llm span, its turn, and its embedding. The
	// embedding row is stamped with staleSession on purpose: search must
	// report the span's own session_id.
	insertSpan := func(traceID, spanID, sessionID string, turnStarted time.Time, vec []float32) {
		var session any
		if sessionID != "" {
			session = sessionID
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO `+spansTable+` (trace_id, span_id, session_id, model, input)
			VALUES ($1, $2, $3, 'm', '[{"type":"text","text":"some content"}]'::jsonb)
		`, traceID, spanID, session)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `
			INSERT INTO `+turnsTable+` (trace_id, session_id, user_prompt, started_at)
			VALUES ($1, $2, 'prompt for '||$1, $3)
			ON CONFLICT (trace_id) DO NOTHING
		`, traceID, session, turnStarted)
		Expect(err).NotTo(HaveOccurred())
		Expect(store.UpsertSpanChunks(ctx, spanembed.ChunkRecord{
			TraceID: traceID, SpanID: spanID, SessionID: staleSession,
			Model: "m", ContentHash: "h-" + spanID,
			Embeddings: [][]float32{vec},
		})).To(Succeed())
	}

	spanIDs := func(hits []spanembed.Hit) []string {
		out := make([]string, 0, len(hits))
		for _, h := range hits {
			out = append(out, h.SpanID)
		}
		return out
	}

	search := func(filter spanembed.SearchFilter) []string {
		hits, err := store.Search(ctx, query, 10, filter)
		Expect(err).NotTo(HaveOccurred())
		return spanIDs(hits)
	}

	day := func(d, h int) time.Time { return time.Date(2026, 6, d, h, 0, 0, 0, time.UTC) }

	Context("over a small corpus of three sessions", func() {
		BeforeEach(func() {
			now := time.Now()
			endedAt := now.Add(-time.Hour)
			// Ada: live (not ended, seen just now), no display name.
			insertSession(sessionAda, "user_ada", "/src/app", "claude", now, nil, "", "Fix the retry backoff")
			// Bob: ended an hour ago, with a display name over his title.
			insertSession(sessionBob, "user_bob", "/src/other", "codex", endedAt, &endedAt, "Bob's session", "derived")

			insertSpan("trace_a", "span_a", sessionAda, day(1, 10), []float32{1, 0.1, 0})
			insertSpan("trace_b", "span_b", sessionBob, day(3, 10), []float32{1, 0.2, 0})
			insertSpan("trace_c", "span_c", sessionGone, day(5, 10), []float32{1, 0.3, 0})
		})

		It("returns every span unfiltered, nearest first, with its session context", func() {
			hits, err := store.Search(ctx, query, 10, spanembed.SearchFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(spanIDs(hits)).To(Equal([]string{"span_a", "span_b", "span_c"}))

			a, b, c := hits[0], hits[1], hits[2]
			Expect(a.SessionID).To(Equal(sessionAda), "session_id comes from the span, not the stale embedding row")
			Expect(a.UserPrompt).To(Equal("prompt for trace_a"))
			Expect(a.AuthSubject).To(Equal("user_ada"))
			Expect(a.Cwd).To(Equal("/src/app"))
			Expect(a.HarnessID).To(Equal("claude"))
			Expect(a.DisplayTitle).To(Equal("Fix the retry backoff"), "an empty display name falls back to the derived title")
			Expect(a.LastSeenAt).To(BeTemporally("~", time.Now(), time.Minute))
			Expect(a.Live).To(BeTrue())

			Expect(b.SessionID).To(Equal(sessionBob))
			Expect(b.DisplayTitle).To(Equal("Bob's session"))
			Expect(b.Live).To(BeFalse())

			// A span whose session row is unknown still matches, with empty
			// session context rather than being dropped by the join.
			Expect(c.SessionID).To(Equal(sessionGone))
			Expect(c.AuthSubject).To(BeEmpty())
			Expect(c.DisplayTitle).To(BeEmpty())
			Expect(c.LastSeenAt.IsZero()).To(BeTrue())
			Expect(c.Live).To(BeFalse())
		})

		It("filters by exact auth subject, OR'ing several", func() {
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{"user_bob"}})).To(Equal([]string{"span_b"}))
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{"user_ada", "user_bob"}})).
				To(Equal([]string{"span_a", "span_b"}))
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{"nobody", "user_ada"}})).To(Equal([]string{"span_a"}))
			// Exact: no case folding, no partial match.
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{"USER_BOB"}})).To(BeEmpty())
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{"user"}})).To(BeEmpty())
			// An empty id filters nothing rather than matching subjectless sessions.
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{""}})).To(Equal([]string{"span_a", "span_b", "span_c"}))
		})

		It("filters by working-directory prefix", func() {
			Expect(search(spanembed.SearchFilter{CwdPrefix: "/src/a"})).To(Equal([]string{"span_a"}))
			Expect(search(spanembed.SearchFilter{CwdPrefix: "/src/"})).To(Equal([]string{"span_a", "span_b"}))
			Expect(search(spanembed.SearchFilter{CwdPrefix: "src"})).To(BeEmpty())
		})

		It("filters by harness", func() {
			Expect(search(spanembed.SearchFilter{HarnessID: "codex"})).To(Equal([]string{"span_b"}))
		})

		It("windows on the turn's start, since inclusive and until exclusive", func() {
			since := day(3, 10)
			until := day(3, 10)
			Expect(search(spanembed.SearchFilter{Since: &since})).To(Equal([]string{"span_b", "span_c"}))
			Expect(search(spanembed.SearchFilter{Until: &until})).To(Equal([]string{"span_a"}))

			from, to := day(2, 0), day(4, 0)
			Expect(search(spanembed.SearchFilter{Since: &from, Until: &to})).To(Equal([]string{"span_b"}))
		})

		It("filters by liveness", func() {
			live, notLive := true, false
			Expect(search(spanembed.SearchFilter{Live: &live})).To(Equal([]string{"span_a"}))
			Expect(search(spanembed.SearchFilter{Live: &notLive})).To(Equal([]string{"span_b", "span_c"}))
		})

		It("combines filters", func() {
			since := day(1, 0)
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{"user_ada"}, HarnessID: "claude", CwdPrefix: "/src", Since: &since})).
				To(Equal([]string{"span_a"}))
			Expect(search(spanembed.SearchFilter{AuthSubjects: []string{"user_ada"}, HarnessID: "codex"})).To(BeEmpty())
		})
	})

	It("still fills top_k when the nearest vectors all sit in an excluded session", func() {
		now := time.Now()
		insertSession(sessionAda, "user_ada", "/src/app", "claude", now, nil, "", "")
		insertSession(sessionBob, "user_bob", "/src/other", "codex", now, nil, "", "")

		// 200 near-duplicates of the query in Bob's session — far more than
		// hnsw.ef_search (40), so a non-iterative scan would hand back only
		// Bob's spans and the filter would leave nothing.
		for i := range 200 {
			insertSpan(fmt.Sprintf("trace_bob_%03d", i), fmt.Sprintf("span_bob_%03d", i), sessionBob,
				day(1, 10), []float32{1, 0.001 * float32(i%10), 0.001 * float32(i/10)})
		}
		// Ada's spans are all further from the query than any of Bob's.
		for i := range 8 {
			insertSpan(fmt.Sprintf("trace_ada_%d", i), fmt.Sprintf("span_ada_%d", i), sessionAda,
				day(1, 10), []float32{1, 1 + 0.1*float32(i), 0})
		}

		hits, err := store.Search(ctx, query, 5, spanembed.SearchFilter{AuthSubjects: []string{"user_ada"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(spanIDs(hits)).To(Equal([]string{"span_ada_0", "span_ada_1", "span_ada_2", "span_ada_3", "span_ada_4"}))
		for _, h := range hits {
			Expect(h.AuthSubject).To(Equal("user_ada"))
		}
	})
})
