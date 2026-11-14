package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestLoad_ReadsTheForkLimitsOrTheirDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want config.Forks
	}{
		{"empty file", "", config.Forks{DefaultTTL: 168 * time.Hour, MaxTTL: 720 * time.Hour, MaxPerWorkspace: 200}},
		{"only default_ttl", "[forks]\ndefault_ttl = \"2d\"\n", config.Forks{DefaultTTL: 48 * time.Hour, MaxTTL: 720 * time.Hour, MaxPerWorkspace: 200}},
		{"all three keys", "[forks]\ndefault_ttl = \"2d\"\nmax_ttl = \"10d\"\nmax_per_workspace = 3\n", config.Forks{DefaultTTL: 48 * time.Hour, MaxTTL: 240 * time.Hour, MaxPerWorkspace: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(noEnv(), dirWith(t, tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Forks != tc.want {
				t.Errorf("Forks = %+v, want %+v", cfg.Forks, tc.want)
			}
		})
	}
}

func TestLoad_RefusesForkLimitsThatContradictEachOther(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		key  string // "" when the file is accepted
	}{
		{"default_ttl not a duration", "default_ttl = \"soon\"", "forks.default_ttl"},
		{"negative max_ttl", "max_ttl = \"-1d\"", "forks.max_ttl"},
		{"default_ttl under five minutes", "default_ttl = \"4m\"", "forks.default_ttl"},
		{"max_ttl shorter than default_ttl", "default_ttl = \"8d\"\nmax_ttl = \"7d\"", "forks.max_ttl"},
		{"max_ttl shorter than the default default_ttl", "max_ttl = \"2d\"", "forks.max_ttl"},
		{"no fork allowed", "max_per_workspace = 0", "forks.max_per_workspace"},
		{"equal default_ttl and max_ttl", "default_ttl = \"7d\"\nmax_ttl = \"7d\"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(noEnv(), dirWith(t, "[forks]\n"+tc.text+"\n"))
			switch {
			case tc.key == "" && err != nil:
				t.Fatalf("Load error = %v, want nil", err)
			case tc.key != "" && err == nil:
				t.Fatalf("Load error = nil, want one naming %s", tc.key)
			case tc.key != "" && !strings.Contains(err.Error(), tc.key):
				t.Errorf("Load error = %q, want it to name %s", err, tc.key)
			}
		})
	}
}
