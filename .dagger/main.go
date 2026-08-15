// Search cassette CI/CD.
package main

import (
	"context"

	"dagger/search-cassette/internal/dagger"
)

const imageName = "search-cassette"

type SearchCassette struct {
	// +private
	Source *dagger.Directory
}

func New(
	// Project source directory.
	// +defaultPath="/"
	// +ignore=[".git", ".dagger", ".direnv", ".devenv", "build", "tmp"]
	source *dagger.Directory,
) *SearchCassette {
	return &SearchCassette{Source: source}
}

func (m *SearchCassette) image() *dagger.Dockerimage {
	return dag.Dockerimage(dagger.DockerimageOpts{Source: m.Source})
}

// Test runs the test suites with a just-in-time Postgres service exposed
// as TEST_POSTGRES_DSN, so the DB-backed specs run against the real
// engine instead of skipping.
//
// +check
func (m *SearchCassette) Test(ctx context.Context) (string, error) {
	return dag.Container().
		From("golang:1.26-bookworm").
		WithEnvVariable("CGO_ENABLED", "0").
		WithEnvVariable("GOEXPERIMENT", "jsonv2").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("go-build")).
		WithWorkdir("/src").
		WithDirectory("/src", m.Source).
		WithServiceBinding("postgres", postgresService()).
		WithEnvVariable("TEST_POSTGRES_DSN", newPostgresDSN()).
		WithExec([]string{"go", "test", "-count=1", "-v", "./..."}).
		Stdout(ctx)
}

// BuildImage builds the local-platform search cassette image.
func (m *SearchCassette) BuildImage() *dagger.Container {
	return m.image().Build()
}

// BuildPushImage builds and publishes the multi-platform search cassette image.
func (m *SearchCassette) BuildPushImage(
	ctx context.Context,

	// Registry namespace, for example public.ecr.aws/example/papercomputeco.
	registry string,

	// Tags to publish, for example ["v1.0.0", "latest"].
	tags []string,
) ([]string, error) {
	return m.image().Publish(ctx, registry+"/"+imageName, tags)
}
