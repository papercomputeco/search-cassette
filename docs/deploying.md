---
title: Deploying
description: The image, its configuration, the embedding providers it supports, and the database objects a deployment must provision.
sidebar:
  order: 4
---

Tapes does not start cassettes. A deployment starts the process, supplies its
configuration and credentials, and tells tapes where to find its OpenAPI document.

```text
public.ecr.aws/g4e5l3z3/papercomputeco/search-cassette:<release-tag>
```

The image tag is the release tag verbatim, `v` and all — copy one from
[releases](https://github.com/papercomputeco/search-cassette/releases) and use it
as written, e.g. `…/search-cassette:v0.2.2`. Pin a release; `nightly` is published
but is not one.

The version is not a number anyone maintains. A release stamps the tag it is
publishing at link time, and the manifest version, the image reference the
manifest advertises, and the OpenAPI info block all derive from it. A source
build reports `0.0.0`, because a source tree is not a release and a
plausible-looking number there would describe one that never happened.

It listens on `9998` by default and serves `/ping`, `/openapi`, and its API under
`/api/search`.

## Configuration

Everything arrives through the environment. Only the database URL is required.

| Variable | Default | Purpose |
| --- | --- | --- |
| `TAPES_DATABASE_URL` | — **(required)** | Postgres credential provisioned for `cassette_search`. |
| `CASSETTE_LISTEN` | `0.0.0.0:9998` | Listener address. |
| `CASSETTE_EMBEDDING_PROVIDER` | `ollama` | `ollama` or `openai`. |
| `CASSETTE_EMBEDDING_TARGET` | provider default | Provider URL. |
| `CASSETTE_EMBEDDING_MODEL` | provider default | Model. Paired with dimensions. |
| `CASSETTE_EMBEDDING_DIMENSIONS` | provider default | Vector column size. |
| `CASSETTE_EMBEDDING_API_KEY` | — | Secret. Required for OpenAI. |
| `CASSETTE_EMBED_INTERVAL` | `1m` | Embed pass cadence. |
| `CASSETTE_EMBED_BATCH_SIZE` | `100` | Candidate page size. |
| `CASSETTE_EMBED_MAX_TEXT_BYTES` | `1048576` | Per-span rendered-text cap. |
| `CASSETTE_DB_SCHEMA` | the cassette name (`search` unless changed) | Cassette-owned schema for the embedding tables. |
| `CASSETTE_SPANS_TABLE` | `tapes_v1.spans` | Span projection relation. |
| `CASSETTE_SPAN_TURNS_TABLE` | `tapes_v1.span_turns` | Span-turn projection relation. |
| `CASSETTE_WAIT_FOR_DB` | `false` | Retry an unreachable Postgres at startup with backoff instead of exiting. |

These names are mechanical: each is the manifest's config key under the standard
`key` → `CASSETTE_KEY` conversion, so `embedding.model` becomes
`CASSETTE_EMBEDDING_MODEL`.

**Never point the table settings at a date-versioned physical table.** The
`tapes_v1` views hold their names across tapes' projection-generation rotations;
the tables underneath do not.

## Providers

| Provider | Default target | Default model | Default dimensions |
| --- | --- | --- | --- |
| `ollama` | `http://localhost:11434` | `embeddinggemma` | 768 |
| `openai` | `https://api.openai.com` | `text-embedding-3-large` | 1024 |

Dimensions must match the model's output. The cassette fails fast at startup when
the configured dimensions disagree with an existing embedding table, rather than
writing vectors that can never match — changing model or dimensions means
re-embedding, not a restart.

## Database

The manifest declares both halves of what this cassette needs:

```toml
[depends]
core = "v1"
views = ["spans", "span_turns"]

[[tables]]
name = "span_embeddings"

[[tables]]
name = "span_embeddings_failures"
```

It reads tapes' contract views, and it owns its embedding tables in its own schema.
Core publishes the declaration and grants nothing; provisioning is the deployment's
job.

The names are derived, not chosen: role is `cassette_` + the installed name, schema
is the installed name.

```sql
CREATE EXTENSION IF NOT EXISTS vector;
CREATE ROLE "cassette_search" LOGIN PASSWORD '…';

-- Lets the cassette create and own its own schema; it runs its own
-- migrations inside it at startup, including the pgvector HNSW index.
GRANT CREATE ON DATABASE tapes TO "cassette_search";

-- Read access to exactly the declared contract views.
GRANT USAGE ON SCHEMA tapes_v1 TO "cassette_search";
GRANT SELECT ON tapes_v1.spans, tapes_v1.span_turns TO "cassette_search";
```

**pgvector is a provisioning concern.** A runtime credential commonly cannot create
extensions in managed Postgres, so the cassette checks for it and fails fast rather
than trying.

`provision.sql` in the repository is the example-deployment equivalent, and it takes
a shortcut worth knowing about: Postgres runs init scripts once, before tapes has
migrated, so the `tapes_v1` views do not exist yet and cannot be granted by name. It
uses `ALTER DEFAULT PRIVILEGES` instead, which is wider than the grants above.

**Upgrading an existing volume.** A volume provisioned before the `tapes_v1` views
existed carries public-schema-only privileges, and the init script will not run
again to widen them — reads fail with `permission denied`. Apply the two `GRANT`
statements above once, after tapes has migrated. They are idempotent. Do not reach
for `docker compose down -v`: that deletes the database, raw turns, sessions and
embeddings included.

## Pointing tapes at it

Tapes needs the exact URL of the metadata-bearing OpenAPI document:

```toml
# .tapes/config.toml
cassettes = ["http://127.0.0.1:9998/openapi"]
```

or without editing the config:

```sh
tapes serve --cassettes=http://127.0.0.1:9998/openapi
```

Confirm it resolved:

```sh
curl localhost:8081/v1/cassettes
curl "localhost:8081/v1/cassettes/search/spans?query=retry+backoff&top_k=3"
```

Until the first embed pass finishes there is nothing to match, so this answers
`200` with `count: 0` rather than an error — the table itself is created at
startup. A `503` here means search is not configured or its table is missing; see
[Embedding](./embedding.md) for when results begin appearing.
