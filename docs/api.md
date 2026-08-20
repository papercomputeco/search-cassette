---
title: API reference
description: The span search endpoint, the MCP tool over the same operation, the result shape, and status codes.
sidebar:
  order: 2
---

Paths below are given on the cassette's own listener. Through tapes, replace
`/api/search` with `/v1/cassettes/search`.

## `GET /api/search/spans`

Embeds the query text and runs vector similarity over the embedded span
projection.

| Parameter | Type | Default | Meaning |
| --- | --- | --- | --- |
| `query` | string, **required** | — | Search query text. |
| `top_k` | integer, 1–100 | `5` | Maximum results to return. |

```sh
curl "localhost:8081/v1/cassettes/search/spans?query=retry+backoff&top_k=3"
```

The response carries the query back, the results, and their count:

```json
{
  "query": "retry backoff",
  "results": [
    {
      "session_id": "…",
      "trace_id": "…",
      "span_id": "…",
      "score": 0.82
    }
  ],
  "count": 1
}
```

Each result is one span, not one session — the identifiers are what a client needs
to open the matched turn in context.

## `POST /api/search/spans`

The same operation as an MCP tool. It exists as a separate route because the MCP
extension admits JSON-body POST operations, so this is a thin facade over the
search above rather than a second implementation.

```json
{ "query": "retry backoff", "top_k": 3 }
```

Both arguments mean what they mean above, with the same `top_k` ceiling of 100.

Tapes assigns the tool's qualified name from the cassette's installed name plus the
operation's `x-tapes-mcp.name`, so a deployment that installs this cassette under a
different name gets a correspondingly named tool.

## Status codes

Both routes share them.

| Code | When |
| --- | --- |
| `200` | Search ran. Zero results is a `200` with `count: 0`. |
| `400` | Missing `query`, or a `top_k` outside 1–100. |
| `500` | Search execution failed. |
| `503` | Search is not configured, or one of the relations it reads is missing. |

`503` is the one worth recognizing, and it carries two distinct causes:

- **Not configured** — the process has no embedder or no embedding store. Check
  `TAPES_DATABASE_URL` and the provider settings in [Deploying](./deploying.md).
- **A relation is missing.** A search reads three: the cassette's own embedding
  table, and the two contract views it joins for span and turn context
  (`tapes_v1.spans`, `tapes_v1.span_turns`, or whatever the table settings point
  at). Postgres reports `undefined_table` for any of them and all three surface
  as this one `503`, so check the contract views as well as the cassette's schema
  — the embedding table is created at startup, which makes the views the more
  likely culprit. Neither clears on its own.

The first is a configuration signal and the second is a database one; neither is
a `500` because the process itself is healthy. A query that simply matches nothing
is an ordinary `200` with `count: 0`, including before the first embed pass has
put anything in the table.
