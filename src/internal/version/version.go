// Package version is the single source of truth for the Teamster build
// version. The three variables are populated at build time by install.sh via
// -ldflags -X; an un-stamped build (go build, go test) reports "dev".
package version

import "fmt"

var (
	// Version is the git-describe-derived release string (e.g. "0.1.0-3-gabc123").
	Version = "dev"
	// Commit is the short commit hash the binary was built from.
	Commit = "none"
	// BuildTime is the UTC RFC3339 timestamp of the build.
	BuildTime = "unknown"
)

// String renders the full version line: "Version (Commit, BuildTime)".
func String() string {
	return fmt.Sprintf("%s (%s, %s)", Version, Commit, BuildTime)
}
