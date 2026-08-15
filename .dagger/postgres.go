package main

import (
	"fmt"

	"dagger/search-cassette/internal/dagger"
)

// The Postgres service the Test check binds for the DB-backed suite. The
// image is the same pgvector-capable build the compose loop and the
// deployed tenant databases run, so the SQL under test executes against
// the engine it ships to.
const (
	postgresImage = "public.ecr.aws/g4e5l3z3/papercomputeco/postgres:17.7-pgduckdb-1.1.1"
	testPgUser    = "cassette"
	testPgPass    = "cassette"
	testPgDB      = "cassette"
	testPgPort    = 5432
)

// newPostgresDSN returns the connection string for the bound Postgres
// service, addressed by its service-binding hostname.
func newPostgresDSN() string {
	return fmt.Sprintf("host=postgres user=%s password=%s dbname=%s port=%d sslmode=disable",
		testPgUser, testPgPass, testPgDB, testPgPort)
}

// postgresService provides a ready-to-run Postgres with the test user,
// password, and database.
func postgresService() *dagger.Service {
	return dag.Container().From(postgresImage).
		WithEnvVariable("POSTGRES_USER", testPgUser).
		WithEnvVariable("POSTGRES_PASSWORD", testPgPass).
		WithEnvVariable("POSTGRES_DB", testPgDB).
		WithExposedPort(testPgPort).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true})
}
