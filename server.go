package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/papercomputeco/search-cassette/internal/embeddings"
	"github.com/papercomputeco/search-cassette/internal/spanembed"
)

// maxTopK bounds a search request's result count, mirroring the max page
// size of tapes' keyset-paginated list endpoints. An unbounded top_k would
// let one request buy an arbitrarily expensive vector scan — and overflow
// the store's overfetch multiplication into a negative LIMIT.
const maxTopK = 100

// Span search — "find the turn where X happened". The query text is
// embedded and matched against the span-embedding projection (main
// llm spans only, delta-only content), and every hit carries its
// span→trace→turn context so a client can jump straight to the turn.

// SpanSearchResult is one span hit with its trace/turn context.
type SpanSearchResult struct {
	TraceID   string  `json:"trace_id"`
	SpanID    string  `json:"span_id"`
	SessionID string  `json:"session_id,omitempty"`
	Score     float32 `json:"score"`
	// UserPrompt is the prompt of the turn (trace) the span belongs to.
	// Served explicitly (not omitempty) so a synthetic turn's empty prompt
	// reaches consumers as "" rather than a dropped key.
	UserPrompt string `json:"user_prompt"`
	// Snippet previews the matched span's delta-only text.
	Snippet   string    `json:"snippet,omitempty"`
	Model     string    `json:"model,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// SpanSearchOutput is the span search response.
type SpanSearchOutput struct {
	Query   string             `json:"query"`
	Results []SpanSearchResult `json:"results"`
	Count   int                `json:"count"`
}

type mcpSearchRequest struct {
	Query *string         `json:"query"`
	TopK  json.RawMessage `json:"top_k,omitempty"`
}

type mcpSearchResult struct {
	SessionID  string    `json:"session_id,omitempty"`
	TraceID    string    `json:"trace_id"`
	SpanID     string    `json:"span_id"`
	Score      float32   `json:"score"`
	UserPrompt string    `json:"user_prompt,omitempty"`
	Snippet    string    `json:"snippet,omitempty"`
	Model      string    `json:"model,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

type mcpSearchOutput struct {
	Query   string            `json:"query"`
	Results []mcpSearchResult `json:"results"`
	Count   int               `json:"count"`
}

// ErrorResponse is the error body every non-200 response carries — the same
// shape tapes' API serves.
type ErrorResponse struct {
	Error string `json:"error"`
}

// SpanSearcher runs vector similarity over the embedded span projection.
// *spanembed.Store implements it; tests substitute a fake.
type SpanSearcher interface {
	Search(ctx context.Context, embedding []float32, topK int) ([]spanembed.Hit, error)
}

// server is the cassette's HTTP surface: the two root anchors core probes
// (/ping, /openapi), the ops endpoints (/healthz, /readyz, /metrics), and the
// search API under the declared prefix.
type server struct {
	name     string
	embedder embeddings.Embedder
	searcher SpanSearcher
	ready    func(ctx context.Context) error
	metrics  http.Handler
	logger   *slog.Logger
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	// Anchors. These live at the root of the listener because they describe
	// the process, not the API — core probes and fetches them directly and
	// never proxies them.
	mux.HandleFunc("GET /ping", s.handlePing)
	mux.HandleFunc("GET /openapi", s.handleOpenAPI)

	// Ops surface for the deployment, mirroring the tapes embed worker's
	// listener: liveness never depends on the database, readiness does.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}

	// The API itself, under the prefix clients call through tapes:
	// /api/search/spans here is /v1/cassettes/search/spans publicly.
	prefix := "/api/" + s.name
	mux.HandleFunc("GET "+prefix+"/spans", s.handleSearchSpans)
	mux.HandleFunc("POST "+prefix+"/spans", s.handleMCPSearch)

	return mux
}

func (s *server) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "cassette": s.name})
}

func (s *server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPIDocument(s.name))
}

func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.ready == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.ready(probeCtx); err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleSearchSpans handles GET /api/<name>/spans — the extracted form of
// tapes' GET /v1/search/spans, with identical parameters, response shape, and
// status semantics.
func (s *server) handleSearchSpans(w http.ResponseWriter, r *http.Request) {
	if s.searcher == nil || s.embedder == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "span search is not configured: embedder and span embedding store are required",
		})
		return
	}

	query := r.URL.Query().Get("query")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "query parameter is required",
		})
		return
	}

	topK := 5
	if topKStr := r.URL.Query().Get("top_k"); topKStr != "" {
		parsed, err := strconv.Atoi(topKStr)
		if err != nil || parsed <= 0 || parsed > maxTopK {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: fmt.Sprintf("top_k must be a positive integer no greater than %d", maxTopK),
			})
			return
		}
		topK = parsed
	}

	output, err := s.searchSpans(r.Context(), query, topK)
	if errors.Is(err, spanembed.ErrNotInitialized) {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, output)
}

// handleMCPSearch is the JSON-body POST facade required by x-tapes-mcp.
func (s *server) handleMCPSearch(w http.ResponseWriter, r *http.Request) {
	if s.searcher == nil || s.embedder == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "span search is not configured: embedder and span embedding store are required",
		})
		return
	}

	var input mcpSearchRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid search arguments: " + err.Error()})
		return
	}
	if input.Query == nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "query is required"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "body must contain one JSON object"})
		return
	}

	topK := 5
	if input.TopK != nil {
		if bytes.Equal(bytes.TrimSpace(input.TopK), []byte("null")) || json.Unmarshal(input.TopK, &topK) != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "top_k must be an integer"})
			return
		}
		if topK <= 0 {
			topK = 5
		}
	}
	output, err := s.searchSpans(r.Context(), *input.Query, topK)
	if errors.Is(err, spanembed.ErrNotInitialized) {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	results := make([]mcpSearchResult, len(output.Results))
	for i, result := range output.Results {
		results[i] = mcpSearchResult{
			SessionID: result.SessionID, TraceID: result.TraceID, SpanID: result.SpanID,
			Score: result.Score, UserPrompt: result.UserPrompt, Snippet: result.Snippet,
			Model: result.Model, StartedAt: result.StartedAt,
		}
	}
	writeJSON(w, http.StatusOK, mcpSearchOutput{Query: output.Query, Results: results, Count: output.Count})
}

func (s *server) searchSpans(ctx context.Context, query string, topK int) (SpanSearchOutput, error) {
	embedding, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return SpanSearchOutput{}, fmt.Errorf("failed to embed query: %w", err)
	}
	hits, err := s.searcher.Search(ctx, embedding, topK)
	if err != nil {
		return SpanSearchOutput{}, err
	}

	results := make([]SpanSearchResult, 0, len(hits))
	for _, h := range hits {
		results = append(results, SpanSearchResult{
			TraceID:    h.TraceID,
			SpanID:     h.SpanID,
			SessionID:  h.SessionID,
			Score:      h.Score,
			UserPrompt: h.UserPrompt,
			Snippet:    h.Snippet,
			Model:      h.Model,
			StartedAt:  h.StartedAt,
		})
	}
	return SpanSearchOutput{Query: query, Results: results, Count: len(results)}, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
