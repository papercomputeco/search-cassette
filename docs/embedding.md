---
title: Embedding
description: The embed pass that keeps the span projection searchable — how it selects work, how it handles failure, and the metrics it exposes.
sidebar:
  order: 3
---

Search answers over embedded spans, and the embed pass is what puts them there. It
runs in-process on an interval, and it is designed so that running it more often is
cheap and running it twice is harmless.

## What a pass does

Each pass walks candidate spans from the tapes projection in pages, renders each
span's text, and embeds what has changed.

- **Delta-only rendering.** A span's text is what that turn added, not the whole
  conversation replayed. This is the same normalization tapes derives with, so the
  two cannot drift.
- **Content hashing.** Rendered text is hashed per span, and a span whose hash is
  unchanged since its last embedding is skipped. Re-running a pass over unchanged
  data embeds nothing. The comparison is within a span's own identity — identical
  text in two different spans is embedded twice.
- **Chunking.** A span too large for one embedding is split into chunks.
- **Orphan pruning.** Embeddings whose spans no longer exist are removed, so
  re-derivation on the tapes side does not leave the index carrying rows that can
  never match anything real.

Because identity is `(trace_id, span_id)` and hashing is deterministic, a pass is
idempotent: interrupt one and the next resumes rather than repeating work.

## Failure handling

A span that fails to embed deterministically — it will fail the same way every time,
such as text that no chunking can make acceptable — is recorded in a failures
sidecar rather than retried forever. That is what "poisoned" means in the metrics
below: the pass has decided not to spend the next pass on it.

A span that exceeds `CASSETTE_EMBED_MAX_TEXT_BYTES` is recorded as `too_large`
rather than chunked. Set the guard negative to disable it.

Transient failures — the provider is down, the network blipped — are not poisoned.
The loop backs off and the next pass picks the span up again.

## Tuning

| Variable | Default | Effect |
| --- | --- | --- |
| `CASSETTE_EMBED_INTERVAL` | `1m` | How often a full pass runs. |
| `CASSETTE_EMBED_BATCH_SIZE` | `100` | Candidate page size. Bounds peak memory per pass. |
| `CASSETTE_EMBED_MAX_TEXT_BYTES` | `1048576` | Per-span rendered-text cap. Negative disables. |

The interval is measured from the **end** of one pass to the start of the next, so
a pass's own duration adds to the gap between starts. An overlong pass is followed
by another full interval rather than running back to back — worth knowing when
sizing the interval against how fresh search results need to be.

Lower the batch size if peak memory matters more than throughput.

## Metrics

Prometheus metrics are exposed under the `tapes_embed_worker_*` prefix.

| Metric | Reads as |
| --- | --- |
| `passes_total` | Passes started. |
| `pass_duration_seconds` | Pass wall time. |
| `last_success_timestamp_seconds` | When a pass last completed. |
| `consecutive_pass_failures` | Passes failed in a row. |
| `spans_scanned_total` | Candidates examined. |
| `spans_embedded_total` | Spans embedded. |
| `spans_up_to_date_total` | Candidates skipped on an unchanged hash. |
| `spans_chunked_total` / `chunk_rows_total` | Spans split, and rows written for them. |
| `spans_failed_total` / `span_failures_total` | Failures, and failure events. |
| `spans_poisoned_total` | Spans recorded as deterministically failing. |
| `spans_empty_total` | Spans that rendered to nothing. |
| `spans_oversize_total` / `oversize_tokens` | Spans over the byte cap, and their size. |
| `orphans_pruned_total` | Embeddings removed for spans that no longer exist. |

The two worth alerting on are `consecutive_pass_failures` climbing and
`last_success_timestamp_seconds` going stale. Watch them together: a worker that
fails every pass and a worker with nothing to do both leave the embedded counters
flat, and only these two tell them apart.
