# search-cassette

The tapes span-search surface, extracted into a
[cassette](https://github.com/papercomputeco/tapes/blob/main/docs/src/cassettes.md):
an independently deployed HTTP service that tapes admits from its OpenAPI
document and reverse-proxies under `/v1/cassettes/search`.

It is a 1:1 port of three things tapes core ships today:

| tapes core | this cassette |
| --- | --- |
| `GET /v1/search/spans` | `GET /v1/cassettes/search/spans` (served locally as `GET /api/search/spans`) |
| MCP `search` tool | cassette-advertised MCP `search.search` tool (served locally as `POST /api/search/spans`) |
| `tapes serve embed-worker` | the embed pass loop, run in-process on its own interval |

The MCP tool accepts the same `query` and optional `top_k` arguments and returns
that same span-shaped structured result. The cassette-qualified name is assigned
by tapes from the cassette name plus the operation's `x-tapes-mcp.name`.

Same query parameters (`query`, `top_k`), same response shape
(`SpanSearchOutput`), same status semantics (400/500/503), same embed-pass
behavior (idempotent, content-hash gated, chunking, deterministic-failure
poisoning, orphan pruning), and the same Prometheus metric names
(`tapes_embed_worker_*`). Nothing has been removed from tapes; this cassette
is the extraction target running alongside it.

One deliberate divergence: tapes#276 (v0.30.1) removed the organization
concept, so this cassette is org-free from birth. Span identity is
(`trace_id`, `span_id`) everywhere — its own tables carry no `org_id`
column, and its reads of the span projection never reference one, so the
cassette works identically before and after tapes ships the `org_id`
DROP COLUMN migrations.

## What was extracted from where

Search-owned code moved here (copied from tapes, now owned by this module):

- `internal/spanembed` ← tapes `pkg/spanembed` — candidate selection,
  delta-only text rendering, content hashing, chunk splitting, the embed
  pass, and the pgvector store.
- `internal/embedworker` ← tapes `pkg/embedworker` — the interval /
  backoff / drain loop and its Prometheus surface.
- `internal/embeddings` ← tapes `pkg/embeddings` — the `Embedder` contract,
  structured provider errors, and the Ollama/OpenAI clients.

Shared content-model code is imported from the tapes module rather than
copied, so rendering and hashing can never drift from what tapes derives:

- `pkg/llm` (content blocks), `pkg/merkle` (harness-tag stripping and
  embed normalization), and `pkg/tapesoapi` (the OpenAPI toolkit).

Deliberate adaptations, all documented inline:

- The embedding tables live in the cassette-owned `search` schema
  (`search.span_embeddings`, `search.span_embeddings_failures`) per the
  cassette contract, instead of the default search path. They are created
  org-free; a pre-cassette tapes `span_embeddings` table is not
  layout-compatible and is re-embedded rather than reused.
- The span projection relations the store reads are configurable
  (`CASSETTE_SPANS_TABLE`, `CASSETTE_SPAN_TURNS_TABLE`), defaulting to the
  `tapes_v1.spans` / `tapes_v1.span_turns` contract views the manifest
  declares. The views hold their names across tapes' projection-generation
  rotations; never point the config at a date-versioned physical table.
- Search and embedding share one process here because both *are* the search
  feature; tapes' derivation stays its own failure domain either way.
- Configuration is environment-only, per the cassette convention — the
  `CASSETTE_*` names below are the manifest's config keys under the standard
  `key -> CASSETTE_KEY` conversion.

## Configuration

Everything arrives through the environment; only the database URL is
required.

| Variable | Default | Purpose |
| --- | --- | --- |
| `TAPES_DATABASE_URL` | — (required) | Postgres credential the deployment provisioned for `cassette_search` |
| `CASSETTE_LISTEN` | `0.0.0.0:9998` | Listener address |
| `CASSETTE_EMBEDDING_PROVIDER` | `ollama` | `ollama` or `openai` |
| `CASSETTE_EMBEDDING_TARGET` | provider default | Provider URL |
| `CASSETTE_EMBEDDING_MODEL` | provider default | Model; paired with dimensions |
| `CASSETTE_EMBEDDING_DIMENSIONS` | provider default | Vector column size; fail-fast on mismatch |
| `CASSETTE_EMBEDDING_API_KEY` | — | Secret; required for OpenAI |
| `CASSETTE_EMBED_INTERVAL` | `1m` | Embed pass cadence |
| `CASSETTE_EMBED_BATCH_SIZE` | `100` | Candidate page size |
| `CASSETTE_EMBED_MAX_TEXT_BYTES` | `1048576` | Per-span rendered-text cap |
| `CASSETTE_DB_SCHEMA` | `search` | Cassette-owned schema for the embedding tables |
| `CASSETTE_SPANS_TABLE` | `tapes_v1.spans` | Span projection relation |
| `CASSETTE_SPAN_TURNS_TABLE` | `tapes_v1.span_turns` | Span-turn projection relation |
| `CASSETTE_WAIT_FOR_DB` | `false` | Retry an unreachable Postgres at startup |

Provider defaults mirror tapes: Ollama is
`http://localhost:11434` / `embeddinggemma` / 768; OpenAI is
`https://api.openai.com` / `text-embedding-3-large` / 1024.

## Run it

The whole stack — Postgres with pgvector, Ollama, tapes, and the cassette:

```bash
docker compose up --build -d
docker compose exec ollama ollama pull embeddinggemma

curl localhost:8081/v1/cassettes
curl localhost:8081/v1/cassettes/search/openapi.json
curl "localhost:8081/v1/cassettes/search/spans?query=retry+backoff&top_k=3"
```

Or against an existing tapes database:

```bash
TAPES_DATABASE_URL=postgres://cassette_search:...@host:5432/tapes \
  ./build/search-cassette
```

then register `http://127.0.0.1:9998/openapi` in the tapes API server's
`cassettes` configuration.

The deployment owns what the manifest declares (see `provision.sql`): the
`cassette_search` role and credential, the pgvector extension, SELECT on the
span projection, and CREATE for the cassette's own schema. The cassette runs
its own migrations at startup and fails fast when the configured dimensions
disagree with an existing table.

## Development

```bash
make help
make build   # GOEXPERIMENT=jsonv2, inherited from the tapes module
make test    # Ginkgo/Gomega suites, including the manifest digest parity test
```

`cassette.toml` and the manifest embedded in `openapi.go` are one schema in
two encodings; `manifest_test.go` fails the build the moment their canonical
digests drift apart.
