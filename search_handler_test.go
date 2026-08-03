package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/papercomputeco/search-cassette/internal/spanembed"
)

// fakeSpanSearcher implements SpanSearcher in memory.
type fakeSpanSearcher struct {
	hits []spanembed.Hit
	err  error
}

func (f *fakeSpanSearcher) Search(_ context.Context, _ []float32, _ int) ([]spanembed.Hit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// mockEmbedder returns a fixed vector for any input.
type mockEmbedder struct{}

func (mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

func (mockEmbedder) Close() error { return nil }

var _ = Describe("handleSearchSpans", func() {
	var (
		handler  http.Handler
		searcher *fakeSpanSearcher
	)

	newServer := func(s *server) http.Handler {
		s.name = "search"
		s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		return s.routes()
	}

	get := func(h http.Handler, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	BeforeEach(func() {
		searcher = &fakeSpanSearcher{}
		handler = newServer(&server{embedder: mockEmbedder{}, searcher: searcher})
	})

	It("returns 503 when span search is not configured", func() {
		bare := newServer(&server{})
		rec := get(bare, "/api/search/spans?query=x")
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
	})

	It("returns 503 when no embed pass has initialized the store", func() {
		searcher.err = spanembed.ErrNotInitialized
		rec := get(handler, "/api/search/spans?query=x")
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
	})

	It("returns 400 when the query parameter is missing", func() {
		rec := get(handler, "/api/search/spans")
		Expect(rec.Code).To(Equal(http.StatusBadRequest))
	})

	It("returns 400 when top_k is not a positive integer", func() {
		rec := get(handler, "/api/search/spans?query=x&top_k=-2")
		Expect(rec.Code).To(Equal(http.StatusBadRequest))
	})

	It("returns hits with their trace/turn context", func() {
		startedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		searcher.hits = []spanembed.Hit{{
			TraceID:    "trc_req1",
			SpanID:     "llm_req1",
			SessionID:  "5b6f0f8e-2c3a-4ec0-9b6e-000000000001",
			Score:      0.91,
			UserPrompt: "fix the retry backoff",
			Snippet:    "set max-poll-backoff to 30s",
			Model:      "claude-sonnet-4-5",
			StartedAt:  startedAt,
		}}

		rec := get(handler, "/api/search/spans?query=retry+backoff&top_k=3")
		Expect(rec.Code).To(Equal(http.StatusOK))

		var out SpanSearchOutput
		Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())

		Expect(out.Query).To(Equal("retry backoff"))
		Expect(out.Count).To(Equal(1))
		Expect(out.Results).To(HaveLen(1))
		Expect(out.Results[0].TraceID).To(Equal("trc_req1"))
		Expect(out.Results[0].SpanID).To(Equal("llm_req1"))
		Expect(out.Results[0].SessionID).To(Equal("5b6f0f8e-2c3a-4ec0-9b6e-000000000001"))
		Expect(out.Results[0].UserPrompt).To(Equal("fix the retry backoff"))
		Expect(out.Results[0].Snippet).To(ContainSubstring("max-poll-backoff"))
		Expect(out.Results[0].StartedAt).To(Equal(startedAt))
	})

	It("returns 500 on search failures", func() {
		searcher.err = errors.New("pg down")
		rec := get(handler, "/api/search/spans?query=x")
		Expect(rec.Code).To(Equal(http.StatusInternalServerError))
	})

	It("answers the health anchor", func() {
		rec := get(handler, "/ping")
		Expect(rec.Code).To(Equal(http.StatusOK))
	})
})
