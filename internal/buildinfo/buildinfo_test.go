package buildinfo

import (
	"runtime"
	"testing"
)

// The test sets the package-level build values, so it must not run in
// parallel with anything that reads them.
func TestGet_ReportsSetValuesAndDefaultsForUnsetOnes(t *testing.T) {
	tests := []struct {
		name                       string
		version, commit, buildTime string
		wantVersion, wantCommit    string
		wantBuildTime              string
	}{
		{
			name:          "nothing set",
			wantVersion:   "dev",
			wantCommit:    "unknown",
			wantBuildTime: "unknown",
		},
		{
			name:          "only version set",
			version:       "1.4.0",
			wantVersion:   "1.4.0",
			wantCommit:    "unknown",
			wantBuildTime: "unknown",
		},
		{
			name:          "only commit set",
			commit:        "3f2a1bc",
			wantVersion:   "dev",
			wantCommit:    "3f2a1bc",
			wantBuildTime: "unknown",
		},
		{
			name:          "everything set",
			version:       "1.4.0",
			commit:        "3f2a1bc",
			buildTime:     "2026-01-02T03:04:05Z",
			wantVersion:   "1.4.0",
			wantCommit:    "3f2a1bc",
			wantBuildTime: "2026-01-02T03:04:05Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBuildValues(t, tt.version, tt.commit, tt.buildTime)

			info := Get()

			if info.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", info.Version, tt.wantVersion)
			}
			if info.Commit != tt.wantCommit {
				t.Errorf("Commit = %q, want %q", info.Commit, tt.wantCommit)
			}
			if info.BuildTime != tt.wantBuildTime {
				t.Errorf("BuildTime = %q, want %q", info.BuildTime, tt.wantBuildTime)
			}
			if info.GoVersion != runtime.Version() {
				t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
			}
		})
	}
}

func setBuildValues(t *testing.T, version, commit, buildTime string) {
	t.Helper()
	oldVersion, oldCommit, oldBuildTime := Version, Commit, BuildTime
	t.Cleanup(func() { Version, Commit, BuildTime = oldVersion, oldCommit, oldBuildTime })
	Version, Commit, BuildTime = version, commit, buildTime
}
