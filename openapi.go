package main

import (
	"context"
	"strings"

	oas "github.com/papercomputeco/tapes/pkg/tapesoapi"
)

// cassetteVersion is the release identity published in both the manifest and
// the OpenAPI info block.
const cassetteVersion = "0.1.0"

// openAPIDocument renders this cassette's OpenAPI document.
//
// Every path is written under /api/<name>, which is what core's prefix
// admission requires. The operation mirrors tapes' GET /v1/search/spans
// registration one for one — same operation ID, parameters, response schema,
// and status codes — so a client moving from /v1/search/spans to
// /v1/cassettes/search/spans sees the same contract.
//
// The manifest core admits the cassette on rides inside the document as a
// root extension, so there is one artifact to fetch and one thing to
// configure — and so a spec and the metadata describing it can never be
// fetched at two different versions.
func openAPIDocument(name string) []byte {
	prefix := "/api/" + name

	parser := oas.NewParser(oas.WithInfo(oas.Info{
		Title:       "Search Cassette",
		Description: "Semantic span search over the tapes read model, extracted from tapes core.",
		Version:     cassetteVersion,
	}))

	provenance := oas.Provenance{Kind: oas.KindManual, Name: "search cassette"}

	// The manifest is contributed as a root extension on its own fragment.
	// Compile renders root extensions verbatim, so the shape core parses is
	// exactly the shape written here.
	_ = parser.AddFragment(oas.Fragment{
		Provenance: provenance,
		Extensions: map[string]any{"x-tapes-cassette": manifest(name)},
	})

	_ = parser.AddOperation("GET", prefix+"/spans",
		oas.NewOperation("searchSpans").
			Summary("Semantic search over span embeddings").
			Description("Embeds the query text and runs vector similarity over the embedded span "+
				"projection (main llm spans, delta-only content). Each hit carries span, trace, and "+
				"turn context.").
			Tag("search").
			QueryParam("query", oas.String(), oas.ParamRequired(),
				oas.ParamDescription("Search query")).
			QueryParam("top_k", oas.Integer(oas.Minimum(1), oas.Default(5)),
				oas.ParamDescription("Maximum number of results to return")).
			JSONResponse(200, "Search hits", parser.Schema(SpanSearchOutput{})).
			JSONResponse(400, "Missing or invalid query parameters", parser.Schema(ErrorResponse{})).
			JSONResponse(500, "Search execution failed", parser.Schema(ErrorResponse{})).
			JSONResponse(503, "Span search is not configured or not yet initialized", parser.Schema(ErrorResponse{})).
			Build(),
		provenance)

	// Compile is a pure function of what was added above, and every Add here
	// is a literal that cannot fail — so an error would mean this function is
	// wrong, not that the request is. Serving an empty body in that case would
	// hide it; core reports a cassette whose document does not parse, which is
	// the louder and more useful failure.
	compiled, err := parser.Compile(context.Background(), oas.WithTarget(oas.V30))
	if err != nil {
		return []byte(`{"error":"could not compile this cassette's OpenAPI document: ` +
			strings.ReplaceAll(err.Error(), `"`, `'`) + `"}`)
	}

	return compiled.JSON()
}

// manifest is the metadata core admits this cassette on. It must stay in
// lockstep with cassette.toml: the two are one schema in two encodings, and
// the manifest digest test asserts they canonicalize identically.
func manifest(name string) map[string]any {
	return map[string]any{
		"kind": "cassette/v1alpha1",
		"cassette": map[string]any{
			"name":         name,
			"version":      cassetteVersion,
			"display_name": "Span Search",
			"description":  "Semantic span search over the tapes read model, extracted from tapes core.",
			"license":      "Apache-2.0",
			"homepage":     "https://github.com/papercomputeco/search-cassette",
			"image":        "tapes/search-cassette:" + cassetteVersion,
			"port":         9998,
		},
		"depends": map[string]any{
			"core":  "v1",
			"views": []string{"spans", "span_turns"},
		},
		"api": map[string]any{
			"health":      "/ping",
			"openapi":     "/openapi",
			"prefix_path": "api",
		},
		"tables": []map[string]any{
			{"name": "span_embeddings"},
			{"name": "span_embeddings_failures"},
		},
		"config": []map[string]any{
			{
				"key":         "embedding.provider",
				"type":        "string",
				"default":     "ollama",
				"enum":        []string{"ollama", "openai"},
				"description": "Embedding provider type.",
			},
			{
				"key":         "embedding.target",
				"type":        "string",
				"description": "Embedding provider URL. Empty takes the provider default (Ollama: http://localhost:11434, OpenAI: https://api.openai.com).",
			},
			{
				"key":         "embedding.model",
				"type":        "string",
				"description": "Embedding model name. Empty takes the provider default (Ollama: embeddinggemma, OpenAI: text-embedding-3-large).",
			},
			{
				"key":         "embedding.dimensions",
				"type":        "int",
				"min":         1,
				"max":         16000,
				"description": "Embedding dimensionality; must match the model's output. Empty takes the provider default (Ollama: 768, OpenAI: 1024).",
			},
			{
				"key":         "embedding.api_key",
				"type":        "string",
				"secret":      true,
				"description": "Embedding provider API key (required for OpenAI).",
			},
			{
				"key":         "embed.interval",
				"type":        "duration",
				"default":     "1m",
				"description": "How often to run a full span embed pass.",
			},
			{
				"key":         "embed.batch_size",
				"type":        "int",
				"default":     100,
				"min":         1,
				"description": "Candidate page size; bounds peak memory per pass.",
			},
			{
				"key":         "embed.max_text_bytes",
				"type":        "int",
				"default":     1048576,
				"description": "Cap on a span's rendered text; larger spans are recorded as too_large instead of chunked. Negative disables the guard.",
			},
			{
				"key":         "org",
				"type":        "string",
				"description": "Only embed spans belonging to this org UUID (default: all orgs).",
			},
			{
				"key":         "db.schema",
				"type":        "string",
				"default":     "search",
				"description": "Cassette-owned schema the embedding tables live in. Set to \"-\" to use the connection's default search path (legacy tapes layout).",
			},
			{
				"key":         "spans_table",
				"type":        "string",
				"default":     "spans_20260615",
				"description": "Tapes span projection relation to read candidates from; may be schema-qualified.",
			},
			{
				"key":         "span_turns_table",
				"type":        "string",
				"default":     "span_turns_20260615",
				"description": "Tapes span-turn projection relation to read turn context from; may be schema-qualified.",
			},
			{
				"key":         "wait_for_db",
				"type":        "bool",
				"default":     false,
				"description": "Retry an unreachable Postgres at startup with backoff instead of exiting.",
			},
		},
	}
}

// RoutePrefix is the prefix this cassette serves under, exported for tests.
func RoutePrefix(name string) string { return "/api/" + strings.TrimPrefix(name, "/") }
