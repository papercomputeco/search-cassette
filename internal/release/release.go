// Package release carries the identity a released image reports.
//
// The version is not maintained by hand. The release stamps the tag it is
// publishing into Version at link time, and a source tree always reads
// Placeholder, because a source tree is not a release. cassette.toml declares
// the same placeholder, so the manifest's two encodings still canonicalize to
// one digest.
//
// This lives outside package main so release builds can stamp it through a
// stable import-path-qualified linker target.
package release

// Version is the release identity: the manifest's version, the tag in its
// image reference, and the OpenAPI info block.
//
// It must stay a package-level variable because the linker -X flag cannot
// write to a constant.
var Version = Placeholder

// Placeholder is what an unstamped build reports. A version no release can
// produce is the point: it reads as "this came from a source tree", where a
// plausible number would read as a release that never happened.
const Placeholder = "0.0.0"
