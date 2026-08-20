# search-cassette

Semantic span search for [tapes](https://tapes.dev): give it query text, get back
the individual spans that match, with the identifiers to open each one in context.

It is a [cassette](https://tapes.dev/docs/cassettes/) — an independently deployed
HTTP service that tapes discovers from its OpenAPI document and reverse-proxies
under `/v1/cassettes/search`. Two things run in this one process: the search
endpoint (plus an MCP tool over the same operation), and the embed pass that keeps
the span-embedding projection current.

**Documentation:** [tapes.dev/docs/search](https://tapes.dev/docs/search/) — the API
reference, the embed pass and its metrics, deployment and providers, and
development.

## Run it

The whole stack — Postgres with pgvector, Ollama, tapes, and the cassette:

```bash
docker compose up --build -d
docker compose exec ollama ollama pull embeddinggemma

curl localhost:8081/v1/cassettes
curl "localhost:8081/v1/cassettes/search/spans?query=retry+backoff&top_k=3"
```

A `503` is expected until the first embed pass completes.

Or against an existing tapes database:

```bash
TAPES_DATABASE_URL=postgres://cassette_search:...@host:5432/tapes \
  ./build/search-cassette
```

then register `http://127.0.0.1:9998/openapi` in the tapes API server's `cassettes`
configuration.

The deployment owns what the manifest declares (see `provision.sql`): the
`cassette_search` role and credential, the pgvector extension, SELECT on the span
projection, and CREATE for the cassette's own schema. The cassette runs its own
migrations at startup and fails fast when the configured dimensions disagree with an
existing table.

## Develop

```bash
make help
make build   # GOEXPERIMENT=jsonv2, inherited from the tapes module
make test    # Ginkgo/Gomega suites, including the manifest digest parity test
```

## License

Dual-licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT license ([LICENSE-MIT](LICENSE-MIT))

at your option. Unless you explicitly state otherwise, any contribution
intentionally submitted for inclusion in the work by you, as defined in the
Apache-2.0 license, shall be dual licensed as above, without any additional
terms or conditions.
