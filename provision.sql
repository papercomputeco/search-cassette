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
-- initialized. A stale volume will skip it: `docker compose down -v` to
-- reset.

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
-- views (tapes_v1.spans, tapes_v1.span_turns). Those views do not exist yet,
-- so this deployment grants the equivalent physical projection tables
-- instead: default privileges cover the tables tapes' migrations will create
-- after this script runs. Tighten to the two contract views once they land.
GRANT USAGE ON SCHEMA public TO "cassette_search";
ALTER DEFAULT PRIVILEGES FOR ROLE tapes IN SCHEMA public
	GRANT SELECT ON TABLES TO "cassette_search";
