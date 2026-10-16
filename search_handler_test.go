package main

import (
	"bytes"
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
	"github.com/onsi/gomega/gstruct"

	"github.com/papercomputeco/search-cassette/internal/spanembed"
)

// fakeSpanSearcher implements SpanSearcher in memory.
type fakeSpanSearcher struct {
	hits       []spanembed.Hit
	err        error
	lastTopK   int
	lastFilter spanembed.SearchFilter
}

func (f *fakeSpanSearcher) Search(_ context.Context, _ []float32, topK int, filter spanembed.SearchFilter) ([]spanembed.Hit, error) {
	f.lastTopK = topK
	f.lastFilter = filter
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
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/search/spans", bytes.NewBufferString(body)))
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

	It("returns 400 when top_k exceeds the maximum", func() {
		rec := get(handler, "/api/search/spans?query=x&top_k=101")
		Expect(rec.Code).To(Equal(http.StatusBadRequest))

		// An overflow-sized value must be rejected, not multiplied into a
		// negative store limit.
		rec = get(handler, "/api/search/spans?query=x&top_k=4611686018427387903")
		Expect(rec.Code).To(Equal(http.StatusBadRequest))
	})

	It("accepts top_k at the maximum", func() {
		rec := get(handler, "/api/search/spans?query=x&top_k=100")
		Expect(rec.Code).To(Equal(http.StatusOK))
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

	It("serves the cassette-advertised MCP search facade", func() {
		searcher.hits = []spanembed.Hit{{TraceID: "trace-1", SpanID: "span-1"}}
		rec := post(`{"query":"retry backoff","top_k":3}`)
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(searcher.lastTopK).To(Equal(3))

		var out SpanSearchOutput
		Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())
		Expect(out.Query).To(Equal("retry backoff"))
		Expect(out.Results[0].SpanID).To(Equal("span-1"))
		Expect(rec.Body.String()).NotTo(ContainSubstring("user_prompt"))
	})

	It("defaults the MCP tool's top_k to 5", func() {
		Expect(post(`{"query":"x"}`).Code).To(Equal(http.StatusOK))
		Expect(searcher.lastTopK).To(Equal(5))
		Expect(post(`{"query":"x","top_k":-1}`).Code).To(Equal(http.StatusOK))
		Expect(searcher.lastTopK).To(Equal(5))
	})

	It("rejects MCP arguments outside the advertised schema", func() {
		Expect(post(`{"top_k":3}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","unknown":true}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","top_k":null}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","top_k":101}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","top_k":4611686018427387903}`).Code).To(Equal(http.StatusBadRequest))
	})

	It("accepts the MCP top_k maximum", func() {
		Expect(post(`{"query":"x","top_k":100}`).Code).To(Equal(http.StatusOK))
		Expect(searcher.lastTopK).To(Equal(maxTopK))
	})

	It("passes every GET filter through to the searcher", func() {
		rec := get(handler, "/api/search/spans?query=x&auth_subject=user_01&auth_subject=local%3Aada&cwd=%2Fsrc%2Fapp"+
			"&harness=claude&since=2026-06-01T00:00:00Z&until=2026-06-02T00:00:00Z&live=true")
		Expect(rec.Code).To(Equal(http.StatusOK))

		f := searcher.lastFilter
		Expect(f.AuthSubjects).To(Equal([]string{"user_01", "local:ada"}))
		Expect(f.CwdPrefix).To(Equal("/src/app"))
		Expect(f.HarnessID).To(Equal("claude"))
		Expect(f.Since).NotTo(BeNil())
		Expect(*f.Since).To(BeTemporally("==", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)))
		Expect(f.Until).NotTo(BeNil())
		Expect(*f.Until).To(BeTemporally("==", time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)))
		Expect(f.Live).To(gstruct.PointTo(BeTrue()))
	})

	It("searches unfiltered when no GET filter is given", func() {
		Expect(get(handler, "/api/search/spans?query=x").Code).To(Equal(http.StatusOK))
		Expect(searcher.lastFilter).To(Equal(spanembed.SearchFilter{}))
	})

	It("rejects malformed GET since, until, and live", func() {
		Expect(get(handler, "/api/search/spans?query=x&since=yesterday").Code).To(Equal(http.StatusBadRequest))
		Expect(get(handler, "/api/search/spans?query=x&until=2026-06-01").Code).To(Equal(http.StatusBadRequest))
		Expect(get(handler, "/api/search/spans?query=x&live=maybe").Code).To(Equal(http.StatusBadRequest))
	})

	It("passes every MCP filter through to the searcher", func() {
		rec := post(`{"query":"x","auth_subject":"user_01","cwd":"/src",` +
			`"harness":"codex","since":"2026-06-01T00:00:00Z","until":"2026-06-02T00:00:00+02:00","live":false}`)
		Expect(rec.Code).To(Equal(http.StatusOK))

		f := searcher.lastFilter
		Expect(f.AuthSubjects).To(Equal([]string{"user_01"}))
		Expect(f.CwdPrefix).To(Equal("/src"))
		Expect(f.HarnessID).To(Equal("codex"))
		Expect(*f.Since).To(BeTemporally("==", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)))
		Expect(*f.Until).To(BeTemporally("==", time.Date(2026, 6, 1, 22, 0, 0, 0, time.UTC)))
		Expect(f.Live).To(gstruct.PointTo(BeFalse()))
	})

	It("accepts several MCP auth subjects as an array", func() {
		Expect(post(`{"query":"x","auth_subject":["user_01","local:ada"]}`).Code).To(Equal(http.StatusOK))
		Expect(searcher.lastFilter.AuthSubjects).To(Equal([]string{"user_01", "local:ada"}))
	})

	It("rejects an MCP auth_subject that is neither a string nor strings", func() {
		Expect(post(`{"query":"x","auth_subject":7}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","auth_subject":[1,2]}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","auth_subject":null}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","auth_subject":[null]}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","auth_subject":["user_01",null]}`).Code).To(Equal(http.StatusBadRequest))
	})

	It("no longer accepts a name-matching subject filter", func() {
		Expect(post(`{"query":"x","subject":"ada"}`).Code).To(Equal(http.StatusBadRequest))
	})

	It("rejects malformed MCP since, until, and live", func() {
		Expect(post(`{"query":"x","since":"yesterday"}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","until":"2026-06-01"}`).Code).To(Equal(http.StatusBadRequest))
		Expect(post(`{"query":"x","live":"true"}`).Code).To(Equal(http.StatusBadRequest))
	})

	It("serves each hit's session context on both surfaces", func() {
		lastSeen := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
		searcher.hits = []spanembed.Hit{{
			TraceID: "trc", SpanID: "spn", SessionID: "5b6f0f8e-2c3a-4ec0-9b6e-000000000001",
			AuthSubject: "user_01",
			Cwd:         "/src/app", HarnessID: "claude", DisplayTitle: "Fix the backoff",
			LastSeenAt: lastSeen, Live: false,
		}}

		for _, rec := range []*httptest.ResponseRecorder{get(handler, "/api/search/spans?query=x"), post(`{"query":"x"}`)} {
			Expect(rec.Code).To(Equal(http.StatusOK))
			var out struct {
				Results []map[string]any `json:"results"`
			}
			Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())
			Expect(out.Results).To(HaveLen(1))
			hit := out.Results[0]
			Expect(hit).To(HaveKeyWithValue("session_id", "5b6f0f8e-2c3a-4ec0-9b6e-000000000001"))
			Expect(hit).To(HaveKeyWithValue("auth_subject", "user_01"))
			Expect(hit).NotTo(HaveKey("auth_subject_name"))
			Expect(hit).NotTo(HaveKey("auth_subject_email"))
			Expect(hit).To(HaveKeyWithValue("cwd", "/src/app"))
			Expect(hit).To(HaveKeyWithValue("harness_id", "claude"))
			Expect(hit).To(HaveKeyWithValue("display_title", "Fix the backoff"))
			Expect(hit).To(HaveKeyWithValue("last_seen_at", "2026-06-01T12:30:00Z"))
			// live is served even when false.
			Expect(hit).To(HaveKeyWithValue("live", false))
		}
	})

	It("omits unknown session context rather than serving empty values", func() {
		searcher.hits = []spanembed.Hit{{TraceID: "trc", SpanID: "spn"}}
		body := get(handler, "/api/search/spans?query=x").Body.String()
		Expect(body).NotTo(ContainSubstring("auth_subject"))
		Expect(body).NotTo(ContainSubstring("last_seen_at"))
		Expect(body).To(ContainSubstring(`"live":false`))
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
