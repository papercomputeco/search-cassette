IMAGE ?= tapes/search-cassette:0.1.0

# The tapes module builds with the jsonv2 experiment; this module inherits
# that requirement through pkg/merkle.
export GOEXPERIMENT := jsonv2

.PHONY: check
check: ## Runs the Dagger checks
	dagger check

.PHONY: build
build: ## Builds the cassette binary
	go build -o build/search-cassette .

.PHONY: image
image: ## Builds and loads the cassette container image via Dagger
	dagger call build-image export-image --name=$(IMAGE)

.PHONY: check-image
check-image: ## Builds the cassette container image without loading it
	dagger call build-image sync

.PHONY: test
test: ## Runs the test suites
	go test ./...

.PHONY: vet
vet: ## Vets and type-checks
	go vet ./...

.PHONY: format
format: ## Formats and organizes imports
	gofmt -w .
	goimports -local github.com/papercomputeco/search-cassette -w .

.PHONY: clean
clean: ## Removes built artifacts
	rm -rf build/

.PHONY: help
.DEFAULT_GOAL := help
help: ## Prints this help message
	@grep -h -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
