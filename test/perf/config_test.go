package perf_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/perf"
)

func TestConfigFromEnv_SkipsFailsOrReadsTheVariables(t *testing.T) {
	t.Parallel()
	const url, token = "https://hub.example", "parley_secret_token"
	tests := []struct {
		name     string
		env      map[string]string
		skip     string // text of the skip message; empty when the run goes on
		err      string // text of the error; empty when there is none
		want     perf.Config
		wantRead bool
	}{
		{name: "no URL", env: map[string]string{}, skip: "PARLEY_PERF_URL"},
		{name: "no token", env: map[string]string{"PARLEY_PERF_URL": url}, err: "PARLEY_PERF_TOKEN"},
		{name: "no workspace", env: map[string]string{"PARLEY_PERF_URL": url, "PARLEY_PERF_TOKEN": token},
			err: "PARLEY_PERF_WORKSPACE"},
		{name: "workspaces and defaults",
			env: map[string]string{"PARLEY_PERF_URL": url, "PARLEY_PERF_TOKEN": token, "PARLEY_PERF_WORKSPACE": "a, b,c"},
			want: perf.Config{URL: url, Token: token, Workspaces: []string{"a", "b", "c"},
				ReportPath: filepath.Join(os.TempDir(), "parley-perf-report.json")},
			wantRead: true},
		{name: "capacity, sandbox profile and report path",
			env: map[string]string{"PARLEY_PERF_URL": url, "PARLEY_PERF_TOKEN": token, "PARLEY_PERF_WORKSPACE": "a",
				"PARLEY_PERF_CAPACITY": "1", "PARLEY_PERF_SANDBOX_PROFILE": "agent", "PARLEY_PERF_REPORT": "/x/r.json"},
			want: perf.Config{URL: url, Token: token, Workspaces: []string{"a"}, Capacity: true,
				SandboxProfile: "agent", ReportPath: "/x/r.json"},
			wantRead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lookup := func(name string) (string, bool) { v, ok := tt.env[name]; return v, ok }
			cfg, skip, err := perf.ConfigFromEnv(lookup)
			if tt.skip == "" && skip != "" || !strings.Contains(skip, tt.skip) {
				t.Errorf("skip = %q, want one naming %q", skip, tt.skip)
			}
			switch {
			case tt.err == "" && err != nil:
				t.Errorf("err = %v, want none", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Errorf("err = %v, want one naming %s", err, tt.err)
			case err != nil && strings.Contains(err.Error(), token):
				t.Errorf("err = %v, holds the token", err)
			}
			if !tt.wantRead {
				return
			}
			if cfg.URL != tt.want.URL || cfg.Token != tt.want.Token || !slices.Equal(cfg.Workspaces, tt.want.Workspaces) ||
				cfg.Capacity != tt.want.Capacity || cfg.SandboxProfile != tt.want.SandboxProfile ||
				cfg.ReportPath != tt.want.ReportPath {
				t.Errorf("cfg = %+v, want %+v", cfg, tt.want)
			}
			if cfg.ScenarioDeadline <= 0 || cfg.CapacityDeadline <= 0 || cfg.EventWait <= 0 {
				t.Errorf("cfg = %+v, want a scenario deadline, a capacity deadline and an event wait", cfg)
			}
		})
	}
}
