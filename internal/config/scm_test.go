package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestLoad_ReadsTheSCMLimitsOrTheirDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, text string
		want       config.SCM
	}{
		{"empty file", "", config.SCM{ReadTimeout: 30 * time.Second, MaxFileSize: 32 << 20, MaxCommitSize: 32 << 20}},
		{"every key", "[scm]\nread_timeout = \"5s\"\nmax_file_size = \"1KB\"\nmax_commit_size = \"64MB\"\n",
			config.SCM{ReadTimeout: 5 * time.Second, MaxFileSize: 1024, MaxCommitSize: 64 << 20}},
		{"only read_timeout", "[scm]\nread_timeout = \"2m\"\n", config.SCM{ReadTimeout: 2 * time.Minute, MaxFileSize: 32 << 20, MaxCommitSize: 32 << 20}},
		{"only max_commit_size", "[scm]\nmax_commit_size = \"1KB\"\n", config.SCM{ReadTimeout: 30 * time.Second, MaxFileSize: 32 << 20, MaxCommitSize: 1024}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(noEnv(), dirWith(t, tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SCM != tc.want {
				t.Errorf("SCM = %+v, want %+v", cfg.SCM, tc.want)
			}
		})
	}
}

func TestLoad_RefusesAnSCMLimitOfZeroOrBelow(t *testing.T) {
	t.Parallel()
	cases := []struct{ text, key string }{
		{`read_timeout = "0s"`, "scm.read_timeout"},
		{`read_timeout = "-1s"`, "scm.read_timeout"},
		{`max_file_size = "0"`, "scm.max_file_size"},
		{`max_file_size = "0KB"`, "scm.max_file_size"},
		{`max_file_size = "-1MB"`, "scm.max_file_size"},
		{`max_commit_size = "0MB"`, "scm.max_commit_size"},
		{`max_commit_size = "32"`, "scm.max_commit_size"},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(noEnv(), dirWith(t, "[scm]\n"+tc.text+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Load error = %v, want one naming %s", err, tc.key)
			}
		})
	}
}
