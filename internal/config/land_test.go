package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestLoad_ReadsTheLandKeysOrTheirDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, text string
		want       config.Land
	}{
		{"[land] absent", "", config.Land{MaxAttempts: 10, Timeout: 5 * time.Minute, Workers: 8,
			CommitterName: "Parley", CommitterEmail: "parley@localhost"}},
		{"every key at its least", "[land]\nmax_attempts = 1\nworkers = 1\ntimeout = \"1s\"\n" +
			"committer_name = \"Lander\"\ncommitter_email = \"l@example.invalid\"\n",
			config.Land{MaxAttempts: 1, Timeout: time.Second, Workers: 1, CommitterName: "Lander", CommitterEmail: "l@example.invalid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(noEnv(), dirWith(t, tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Land != tc.want {
				t.Errorf("Land = %+v, want %+v", cfg.Land, tc.want)
			}
		})
	}
}

func TestLoad_RefusesALandKeyOutOfItsRange(t *testing.T) {
	t.Parallel()
	cases := []struct{ text, key string }{
		{`max_attempts = 0`, "land.max_attempts"},
		{`max_attempts = -1`, "land.max_attempts"},
		{`workers = 0`, "land.workers"},
		{`timeout = "0s"`, "land.timeout"},
		{`timeout = "soon"`, "land.timeout"},
		{`committer_name = ""`, "land.committer_name"},
		{`committer_email = ""`, "land.committer_email"},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(noEnv(), dirWith(t, "[land]\n"+tc.text+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Load error = %v, want one naming %s", err, tc.key)
			}
		})
	}
}
