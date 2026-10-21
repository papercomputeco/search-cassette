---
title: API reference
description: The span search endpoint, its filters, the MCP tool over the same operation, the result shape, and status codes.
sidebar:
  order: 2
---

Paths below are given on the cassette's own listener. Through tapes, replace
`/api/search` with `/v1/cassettes/search`.

## `GET /api/search/spans`

Embeds the query text and runs vector similarity over the embedded span
projection. The search covers every session in the tapes database, from every
author; the filters narrow it.

| Parameter | Type | Default | Meaning |
| --- | --- | --- | --- |
| `query` | string, **required** | — | Search query text. |
| `top_k` | integer, 1–100 | `5` | Maximum results to return. |
| `auth_subject` | string, repeatable | — | Only sessions whose auth subject is exactly this id. Repeat it to match any of several. |
| `cwd` | string | — | Only sessions whose working directory starts with this prefix. |
| `harness` | string | — | Only sessions from this harness id (`claude`, `codex`, …). |
| `since` | RFC3339 timestamp | — | Only spans whose turn started at or after this time. |
| `until` | RFC3339 timestamp | — | Only spans whose turn started before this time. |
| `live` | boolean | — | `true`: only live sessions. `false`: only sessions that are not live. |

`auth_subject` is the opaque id tapes stamps on a session at ingest, compared
exactly — no case folding, no partial match. The cassette knows nothing about the
people behind the ids; a client that wants to filter by a person resolves that
person to their id(s) first and passes them here.

A session is **live** when it has not ended and was last seen within the past five
minutes — the same rule tapes' session list applies. The `since`/`until` window is
on the turn's start, again matching the session list.

Filters apply inside the vector search, not to its output. A filtered search
first counts, up to a cap, how many embedded spans its filters match. When few
do, it ranks every one of them by distance, so the result is exact. When many
do, it walks the vector index nearest-first with pgvector's iterative scan,
skipping rows the filters reject, so it still returns `top_k` hits when that
many spans match — up to `CASSETTE_SEARCH_MAX_SCAN_TUPLES` index tuples
visited, after which it returns what it found. The cap,
`CASSETTE_SEARCH_EXACT_ROWS`, defaults to where the two strategies cost the
same for the table's size.

```sh
curl "localhost:8081/v1/cassettes/search/spans?query=retry+backoff&top_k=3&auth_subject=user_01&auth_subject=user_02&since=2026-06-01T00:00:00Z"
```

The response carries the query back, the results, and their count:

```json
{
  "query": "retry backoff",
  "results": [
    {
      "trace_id": "…",
      "span_id": "…",
      "session_id": "…",
      "score": 0.82,
      "user_prompt": "fix the retry backoff",
      "snippet": "set max-poll-backoff to 30s…",
      "model": "claude-sonnet-4-5",
      "started_at": "2026-06-01T12:00:00Z",
      "auth_subject": "user_01",
      "cwd": "/src/app",
      "harness_id": "claude",
      "display_title": "Fix the retry backoff",
      "last_seen_at": "2026-06-01T12:30:00Z",
      "live": false
    }
  ],
  "count": 1
}
```

Each result is one span, not one session — the identifiers are what a client needs
to open the matched turn in context. `session_id` is the span's current session.
The session fields (`auth_subject` through `last_seen_at`) are omitted when empty,
including when the span's session is not in the sessions view; `live` is always
present. `display_title` is the session's display name, or its derived title when
it has none.

## `POST /api/search/spans`

The same operation as an MCP tool. It exists as a separate route because the MCP
extension admits JSON-body POST operations, so this is a thin facade over the
search above rather than a second implementation.

```json
{ "query": "retry backoff", "top_k": 3, "auth_subject": ["user_01", "user_02"], "live": true }
```

Every field means what the parameter of the same name means above, with the same
`top_k` ceiling of 100. `auth_subject` is a string or an array of strings; `live` is
a JSON boolean. An unknown field is a `400`.
Results carry the same session fields as the GET response; `user_prompt` is
omitted when empty.

Tapes assigns the tool's qualified name from the cassette's installed name plus the
operation's `x-tapes-mcp.name`, so a deployment that installs this cassette under a
different name gets a correspondingly named tool.

## Status codes

Both routes share them.

| Code | When |
| --- | --- |
| `200` | Search ran. Zero results is a `200` with `count: 0`. |
| `400` | Missing `query`, a `top_k` outside 1–100, a `since` or `until` that is not RFC3339, a `live` that is not a boolean, or (MCP) an
`auth_subject` that is not a string or array of strings. |
| `500` | Search execution failed. |
| `503` | Search is not configured, or one of the relations it reads is missing. |

`503` is the one worth recognizing, and it carries two distinct causes:

- **Not configured** — the process has no embedder or no embedding store. Check
  `TAPES_DATABASE_URL` and the provider settings in [Deploying](./deploying.md).
- **A relation is missing.** A search reads four: the cassette's own embedding
  table, and the three contract views it joins for span, turn, and session
  context (`tapes_v1.spans`, `tapes_v1.span_turns`, `tapes_v1.sessions`, or
  whatever the table settings point at). Postgres reports `undefined_table` for
  any of them and all four surface as this one `503`, so check the contract views as well as the cassette's schema
  — the embedding table is created at startup, which makes the views the more
  likely culprit. Neither clears on its own.

The first is a configuration signal and the second is a database one; neither is
a `500` because the process itself is healthy. A query that simply matches nothing
is an ordinary `200` with `count: 0`, including before the first embed pass has
put anything in the table.
