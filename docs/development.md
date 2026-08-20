---
title: Development
description: Build and test from a checkout, how the module relates to tapes, and what the test suite pins.
sidebar:
  order: 5
---

```sh
make help
make build   # GOEXPERIMENT=jsonv2, inherited from the tapes module
make test    # Ginkgo/Gomega suites, including the manifest digest parity test
```

## Module boundaries

Search-owned code lives here and is owned by this module: candidate selection,
delta-only text rendering, content hashing, chunk splitting, the embed pass and its
pgvector store; the interval/backoff/drain loop and its Prometheus surface; and the
`Embedder` contract with its Ollama and OpenAI clients.

Shared content-model code is **imported from the tapes module rather than copied** —
`pkg/llm` for content blocks, `pkg/merkle` for harness-tag stripping and embed
normalization, `pkg/tapesoapi` for the OpenAPI toolkit.

That split is the point. Rendering and hashing decide what a span's text *is*, and
if this module owned its own copy, an embedding could describe something subtly
different from what tapes derived — a divergence no test here would catch, because
both sides would be internally consistent. Importing makes it impossible.

## What the tests pin

`cassette.toml` and the manifest embedded in `openapi.go` are one schema in two
encodings. `manifest_test.go` parses both and fails the build the moment their
canonical digests drift apart, so a cassette cannot ship publishing metadata that
disagrees with its authored metadata.
