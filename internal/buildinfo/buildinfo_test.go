package buildinfo

import (
	"runtime"
	"testing"
)

func setBuildValues(t *testing.T, version, commit, buildTime string) {
	t.Helper()
	oldV, oldC, oldB := Version, Commit, BuildTime
	t.Cleanup(func() { Version, Commit, BuildTime = oldV, oldC, oldB })
	Version, Commit, BuildTime = version, commit, buildTime
}

func TestGet_DefaultsUnsetValuesAndKeepsSetOnes(t *testing.T) {
	tests := []struct {
		name                   string
		version, commit, built string
		wantV, wantC, wantB    string
	}{
		{"nothing set", "", "", "", "dev", "unknown", "unknown"},
		{"version only", "1.4.0", "", "", "1.4.0", "unknown", "unknown"},
		{"commit only", "", "3f2a1bc", "", "dev", "3f2a1bc", "unknown"},
		{"all set", "1.4.0", "3f2a1bc", "2026-01-02T03:04:05Z", "1.4.0", "3f2a1bc", "2026-01-02T03:04:05Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBuildValues(t, tt.version, tt.commit, tt.built)
			info := Get()
			if info.Version != tt.wantV {
				t.Errorf("Version = %q, want %q", info.Version, tt.wantV)
			}
			if info.Commit != tt.wantC {
				t.Errorf("Commit = %q, want %q", info.Commit, tt.wantC)
			}
			if info.BuildTime != tt.wantB {
				t.Errorf("BuildTime = %q, want %q", info.BuildTime, tt.wantB)
			}
			if info.GoVersion != runtime.Version() {
				t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
			}
		})
	}
}
