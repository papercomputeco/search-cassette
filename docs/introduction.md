---
title: Search cassette
description: Semantic span search over the tapes read model, with the embedding pass that keeps it current.
sidebar:
  order: 1
---

`search-cassette` answers semantic search over tapes sessions. You give it query
text; it embeds the query and runs vector similarity over embedded spans, returning
individual main-conversation LLM spans with their session, trace and span
identifiers, so a client can jump straight to the matched turn.

It is a [cassette](https://tapes.dev/docs/cassettes/) — an independently deployed
HTTP service that tapes discovers from its OpenAPI document and reverse-proxies
under the tapes namespace.

Two things run in this one process, deliberately:

- **Search** — the query endpoint, and an MCP tool over the same operation.
- **The embed pass** — a loop that keeps the span-embedding projection current.

They share a process because both *are* the search feature; splitting them would
give you two deployments that are useless apart. Tapes' own derivation remains its
own failure domain either way, so an embed pass that falls behind degrades search
freshness without touching capture.

## The two addresses

The cassette serves its API under a local prefix on its own listener; tapes
republishes that API under `/v1/cassettes/<name>`:

| On the cassette's own listener | Through tapes |
| --- | --- |
| `GET /api/search/spans` | `GET /v1/cassettes/search/spans` |
| `POST /api/search/spans` | `POST /v1/cassettes/search/spans` (the MCP tool facade) |

Clients use the tapes address. The local one is what tapes itself talks to, and
what you curl to tell a cassette problem apart from a proxying problem — the
cassette does not know tapes exists.

`/ping` and `/openapi` sit outside the prefix. They are the anchors tapes probes
and fetches, not part of the proxied API.

## What gets embedded

Candidates are the spans of the tapes read model, rendered delta-only: a span's
text is what that turn *added*, not the whole conversation replayed. Rendering is
content-hashed, so a pass re-embeds only what changed, and identical content is
never embedded twice.

The projection relations it reads are the `tapes_v1` contract views, whose names
hold stable across tapes' projection-generation rotations. See
[Embedding](./embedding.md) for the pass itself and
[Deploying](./deploying.md) for the grants it needs.

## Next

- [API reference](./api.md) — parameters, the result shape, and status codes.
- [Embedding](./embedding.md) — the pass, its tuning, and its metrics.
- [Deploying](./deploying.md) — image, configuration, database, and providers.
