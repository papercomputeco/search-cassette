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
