package main

import (
	"context"
	"strings"

	oas "github.com/papercomputeco/tapes/pkg/tapesoapi"

	"github.com/papercomputeco/search-cassette/internal/release"
)

const searchToolDescription = "Semantic search over every stored LLM session, from every author, not only your own. " +
	"Use it to find where something was discussed or decided when you do not know which session it was in: " +
	"\"who looked at the retry backoff\", \"the turn where we chose pgvector\", \"my sessions in this repo last week\". " +
	"It embeds the query and runs vector similarity over main-conversation LLM spans; each result is one span with its " +
	"session, trace, and span identifiers, a text snippet, and the session's auth_subject, cwd, " +
	"harness, title, last-seen time, and whether it is live, so a client can jump straight to the matched turn. " +
	"Optional filters narrow it: auth_subject (exact id; pass several to match any of them), cwd (working-directory prefix), " +
	"harness (harness id, e.g. claude or codex), " +
	"since/until (RFC3339 bounds on the turn's start), and live (only running, or only finished, sessions). " +
	"Filtered searches still return top_k hits when enough spans match."

// Filter descriptions, shared by the GET parameters and the MCP body so the
// two surfaces describe one filter set.
const (
	filterAuthSubject = "only sessions whose opaque auth subject id is exactly this value; repeat to match any of several ids"
	filterCwd         = "only sessions whose working directory starts with this prefix"
	filterHarness     = "only sessions from this harness id (e.g. claude, codex)"
	filterSince       = "only spans whose turn started at or after this RFC3339 time"
	filterUntil       = "only spans whose turn started before this RFC3339 time"
	filterLive        = "true: only live sessions (not ended, seen in the last 5 minutes); false: only sessions that are not live"
)

// authSubjectSchema is the MCP body's auth_subject: one id, or several.
func authSubjectSchema() *oas.Schema {
	schema := oas.OneOf(oas.String(), oas.Array(oas.String()))
	schema.Description = "only sessions whose opaque auth subject id is exactly this value, or any of these values"
	return schema
}

// openAPIDocument renders this cassette's OpenAPI document.
//
// Every path is written under /api/<name>, which is what core's prefix
// admission requires. GET mirrors tapes' /v1/search/spans contract; POST is
// the JSON-body facade advertised as the cassette's MCP search tool.
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
		Version:     release.Version,
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
			QueryParam("top_k", oas.Integer(oas.Minimum(1), oas.Maximum(maxTopK), oas.Default(5)),
				oas.ParamDescription("Maximum number of results to return")).
			QueryParam("auth_subject", oas.Array(oas.String()),
				oas.ParamStyle("form", true),
				oas.ParamDescription(filterAuthSubject)).
			QueryParam("cwd", oas.String(),
				oas.ParamDescription(filterCwd)).
			QueryParam("harness", oas.String(),
				oas.ParamDescription(filterHarness)).
			QueryParam("since", oas.String(oas.Format("date-time")),
				oas.ParamDescription(filterSince)).
			QueryParam("until", oas.String(oas.Format("date-time")),
				oas.ParamDescription(filterUntil)).
			QueryParam("live", oas.Boolean(),
				oas.ParamDescription(filterLive)).
			JSONResponse(200, "Search hits", parser.Schema(SpanSearchOutput{})).
			JSONResponse(400, "Missing or invalid query parameters (query, top_k, since, until, live)", parser.Schema(ErrorResponse{})).
			JSONResponse(500, "Search execution failed", parser.Schema(ErrorResponse{})).
			JSONResponse(503, "Span search is not configured or not yet initialized", parser.Schema(ErrorResponse{})).
			Build(),
		provenance)

	// x-tapes-mcp admits JSON-body POST operations, so this is a thin facade
	// over the same search path rather than a second implementation.
	_ = parser.AddOperation("POST", prefix+"/spans",
		oas.NewOperation("searchSpansMCP").
			Summary("Semantic search over stored LLM sessions").
			Description(searchToolDescription).
			Tag("search").
			Extension("x-tapes-mcp", map[string]any{
				"name": "search",
				"annotations": map[string]any{
					"readOnlyHint":   true,
					"idempotentHint": true,
					"openWorldHint":  false,
				},
			}).
			JSONBody("Search arguments", oas.Object(map[string]*oas.Schema{
				"query":        oas.String(oas.Description("the search query text to find relevant spans")),
				"top_k":        oas.Integer(oas.Maximum(maxTopK), oas.Description("number of results to return (default: 5)")),
				"auth_subject": authSubjectSchema(),
				"cwd":          oas.String(oas.Description(filterCwd)),
				"harness":      oas.String(oas.Description(filterHarness)),
				"since":        oas.String(oas.Format("date-time"), oas.Description(filterSince)),
				"until":        oas.String(oas.Format("date-time"), oas.Description(filterUntil)),
				"live":         oas.Boolean(oas.Description(filterLive)),
			}, oas.Required("query"), oas.NoAdditionalProperties())).
			JSONResponse(200, "Search hits", parser.Schema(mcpSearchOutput{})).
			JSONResponse(400, "Invalid search arguments", parser.Schema(ErrorResponse{})).
			JSONResponse(500, "Search execution failed", parser.Schema(ErrorResponse{})).
			JSONResponse(503, "Span search is not configured or not yet initialized", parser.Schema(ErrorResponse{})).
			Build(),
		provenance)

	// Compile is a pure function of what was added above, and every Add here
	// is a literal that cannot fail — so an error would mean this function is
	// wrong, not that the request is. Serving an empty body in that case would
	// hide it; core reports a cassette whose document does not parse, which is
	// the louder and more useful failure.
	compiled, err := parser.Compile(context.Background(), oas.WithTarget(oas.V31))
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
			"version":      release.Version,
			"display_name": "Span Search",
			"description":  "Semantic span search over the tapes read model, extracted from tapes core.",
			"license":      "MIT OR Apache-2.0",
			"homepage":     "https://github.com/papercomputeco/search-cassette",
			"image":        "public.ecr.aws/g4e5l3z3/papercomputeco/search-cassette:v" + release.Version,
			"port":         9998,
		},
		"depends": map[string]any{
			"core":  "v1",
			"views": []string{"spans", "span_turns", "sessions"},
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
				"key":         "db.schema",
				"type":        "string",
				"default":     "search",
				"description": "Cassette-owned schema the embedding tables live in.",
			},
			{
				"key":         "spans_table",
				"type":        "string",
				"default":     "tapes_v1.spans",
				"description": "Tapes span projection relation to read candidates from; may be schema-qualified.",
			},
			{
				"key":         "span_turns_table",
				"type":        "string",
				"default":     "tapes_v1.span_turns",
				"description": "Tapes span-turn projection relation to read turn context from; may be schema-qualified.",
			},
			{
				"key":         "sessions_table",
				"type":        "string",
				"default":     "tapes_v1.sessions",
				"description": "Tapes session relation to read hit ownership, workspace, and liveness from; may be schema-qualified.",
			},
			{
				"key":         "search.max_scan_tuples",
				"type":        "int",
				"default":     40000,
				"min":         1,
				"description": "hnsw.max_scan_tuples for a search: how many index tuples a filtered search may visit while skipping rows its filters reject.",
			},
			{
				"key":         "search.exact_rows",
				"type":        "int",
				"default":     0,
				"min":         -1,
				"description": "Most matching rows a filtered search ranks exactly instead of walking the index. 0 derives it from the embedding table's size; -1 always walks the index.",
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
