// Package buildinfo holds the version, commit and build time a binary was
// built with. Both programs report them: parleyd on GET /version and the
// parley CLI in parley version.
//
// The values are set with the linker:
//
//	go build -ldflags "-X github.com/agent-fox-dev/parley/internal/buildinfo.Version=1.4.0 \
//	  -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=3f2a1bc \
//	  -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=2026-01-02T03:04:05Z"
package buildinfo

import "runtime"

// Values set at build time with -X; empty when the build did not set them.
var (
	Version   string
	Commit    string
	BuildTime string
)

// Info is the build information of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

// Get returns the build information: an unset version reads "dev", an
// unset commit or build time "unknown", and GoVersion is the toolchain that
// built the binary.
func Get() Info {
	return Info{
		Version:   orDefault(Version, "dev"),
		Commit:    orDefault(Commit, "unknown"),
		BuildTime: orDefault(BuildTime, "unknown"),
		GoVersion: runtime.Version(),
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
