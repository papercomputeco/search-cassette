-- Deployment-side provisioning for the search cassette.
--
-- This file is the deployment holding up its end of the manifest. The
-- cassette declares in cassette.toml that it owns a schema with the
-- span_embeddings tables inside it, and that it depends on tapes' span
-- projection views; core reads that declaration, publishes it, and does
-- nothing about it. Creating the role and granting the reads is somebody
-- else's job, and in this example that somebody is Postgres' own init hook.
--
-- The names are not invented here. They are what the manifest derives:
--
--   role   = "cassette_" + name  ->  cassette_search
--   schema = name                ->  search
--
-- Postgres runs this exactly once, when the data directory is first
-- initialized. A stale volume skips it, and the only thing that re-runs it
-- is destroying the volume — see the upgrade note below before reaching
-- for `docker compose down -v`.
--
-- UPGRADING AN EXISTING VOLUME: a volume provisioned before the tapes_v1
-- contract views carries public-schema-only privileges, and this script
-- will not run again to widen them — the cassette's reads fail with
-- permission denied. Apply the one-time grant (after tapes has migrated,
-- so the views exist); it is idempotent and loses nothing:
--
--   GRANT USAGE ON SCHEMA tapes_v1 TO "cassette_search";
--   GRANT SELECT ON tapes_v1.spans, tapes_v1.span_turns TO "cassette_search";
--
-- `docker compose down -v` also "fixes" it, by deleting the database —
-- raw turns, sessions, and embeddings included. That is for throwaway
-- instances only, never an upgrade path.

-- The embedding tables need pgvector. Extension creation is a provisioning
-- concern: the cassette's runtime credential commonly cannot create
-- extensions in managed Postgres, so its EnsureSchema checks and fails fast
-- rather than trying.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE ROLE "cassette_search" LOGIN PASSWORD 'cassette';

-- CREATE on the database lets the cassette create and own its `search`
-- schema; it runs its own migrations inside it at startup.
GRANT CREATE ON DATABASE tapes TO "cassette_search";

-- The manifest's depends.views declares SELECT on the tapes_v1 contract
-- views (tapes_v1.spans, tapes_v1.span_turns). Tapes' migrations create the
-- schema and the views AFTER this init script runs, so neither can be
-- GRANTed by name here. Default privileges bridge the gap: everything the
-- tapes role later creates — the tapes_v1 schema (ON SCHEMAS covers USAGE)
-- and the views in it (ON TABLES covers views) — arrives readable by the
-- cassette. A production deployment (tko) instead grants USAGE on tapes_v1
-- and SELECT on exactly the declared views once they exist; this
-- example-deployment shortcut is wider than that, and the width is the
-- price of a single-pass init script.
ALTER DEFAULT PRIVILEGES FOR ROLE tapes GRANT USAGE ON SCHEMAS TO "cassette_search";
ALTER DEFAULT PRIVILEGES FOR ROLE tapes GRANT SELECT ON TABLES TO "cassette_search";
