// Package buildinfo holds the version, commit and build time a binary was
// built with, for both parleyd and the parley client.
//
// The values are set at link time:
//
//	go build -ldflags "-X github.com/agent-fox-dev/parley/internal/buildinfo.Version=1.4.0 \
//	  -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=3f2a1bc \
//	  -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=2026-01-02T03:04:05Z"
package buildinfo

import "runtime"

// The build values as the linker set them; empty when it did not.
var (
	Version   string
	Commit    string
	BuildTime string
)

// Defaults reported for a build value the linker did not set. A fixed,
// non-empty value is easy to tell apart from a real one.
const (
	defaultVersion   = "dev"
	defaultCommit    = "unknown"
	defaultBuildTime = "unknown"
)

// Info is the build information of the running binary.
type Info struct {
	Version   string
	Commit    string
	BuildTime string
	// GoVersion is the version of the Go toolchain that built the binary.
	GoVersion string
}

// Get returns the build information, with the defaults in place of values
// that were not set. It reads the build values on every call.
func Get() Info {
	return Info{
		Version:   orDefault(Version, defaultVersion),
		Commit:    orDefault(Commit, defaultCommit),
		BuildTime: orDefault(BuildTime, defaultBuildTime),
		GoVersion: runtime.Version(),
	}
}

func orDefault(value, def string) string {
	if value == "" {
		return def
	}
	return value
}
