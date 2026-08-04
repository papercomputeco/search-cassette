package main

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSearchCassette(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Search Cassette Suite")
}
