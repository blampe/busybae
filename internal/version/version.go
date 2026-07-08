// Package version exposes link-time build metadata.
//
// The values here are stamped in by rules_go's x_defs attribute at
// //:busybae, sourced from the workspace_status_command in
// tools/build/status.sh. Under `bazel build --stamp` (the default in
// .bazelrc), a tagged build produces Version="0.1.2"; an unstamped or
// dev build sees "dev".
//
// A `go build` or `go test` invocation leaves both vars at their
// declared defaults, which is fine for the test suite but means
// install-wrapper needs to be told the version explicitly (or the
// binary needs to be produced by Bazel).
package version

// Version is the release tag this binary was cut from (without the
// leading "v"), or "dev" for unstamped builds.
var Version = "dev"

// Commit is the git SHA at build time, or "unknown".
var Commit = "unknown"

// IsRelease reports whether the binary was stamped with a real version.
func IsRelease() bool {
	return Version != "" && Version != "dev"
}
