// Package version holds the build identity shared by the gpsync CLI and
// the gpsync-tray Windows tray app. Neither binary can import the other
// (both are package main), so this package is what keeps their --version
// output consistent.
//
// The values are placeholders for `go build` and `go run`; release builds
// stamp them with -ldflags -X, for example:
//
//	go build -ldflags "-X github.com/gmizrahi/gpsync/internal/version.Version=1.2.3" ./cmd/gpsync
package version

var (
	// Version is the release version, or "dev" for an unstamped build.
	Version = "dev"
	// Commit is the git revision the binary was built from.
	Commit = "unknown"
	// Date is the build timestamp in RFC 3339 format.
	Date = "unknown"
)

// String renders the full build identity for --version output.
func String() string {
	if Commit == "unknown" {
		return Version
	}
	return Version + " (" + Commit + ", built " + Date + ")"
}
