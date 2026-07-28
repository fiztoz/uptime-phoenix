// Package version carries the build-time version stamp for Phoenix.
//
// Version defaults to "dev" and is stamped by the release build via:
//
//	go build -ldflags "-X github.com/fiztoz/uptime-phoenix/internal/version.Version=<v>"
//
// That exact -X path is a contract with the Makefile/CI release wiring —
// do not move or rename this variable without updating both.
package version

// Version is the semantic version of this build ("dev" when unstamped).
var Version = "dev"
