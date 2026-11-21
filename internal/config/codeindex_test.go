package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestLoad_ReadsTheCodeIndexBoundsOrTheirDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, text string
		want       config.CodeIndex
	}{
		{"no section", "", config.CodeIndex{}},
		{"an empty section", "[code_index]\n",
			config.CodeIndex{Enabled: true, MaxFiles: 100000, MaxSize: 1 << 30, BuildTimeout: 5 * time.Minute, Workers: 2}},
		{"every key", "[code_index]\nmax_files = 7\nmax_size = \"2MB\"\nbuild_timeout = \"90s\"\nworkers = 16\n",
			config.CodeIndex{Enabled: true, MaxFiles: 7, MaxSize: 2 << 20, BuildTimeout: 90 * time.Second, Workers: 16}},
		{"only workers", "[code_index]\nworkers = 1\n",
			config.CodeIndex{Enabled: true, MaxFiles: 100000, MaxSize: 1 << 30, BuildTimeout: 5 * time.Minute, Workers: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(noEnv(), dirWith(t, tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CodeIndex != tc.want {
				t.Errorf("CodeIndex = %+v, want %+v", cfg.CodeIndex, tc.want)
			}
		})
	}
}

func TestLoad_RefusesACodeIndexSectionItCannotUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want []string
	}{
		{`unknown = 1`, []string{"code_index.unknown"}},
		{`max_files = 0`, []string{"code_index.max_files"}},
		{`max_files = "many"`, []string{"code_index.max_files"}},
		{`workers = 0`, []string{"code_index.workers"}},
		{`workers = 17`, []string{"code_index.workers"}},
		{`max_size = "big"`, []string{"code_index.max_size"}},
		{`max_size = "0GB"`, []string{"code_index.max_size"}},
		{`build_timeout = "soon"`, []string{"code_index.build_timeout"}},
		{`build_timeout = "0s"`, []string{"code_index.build_timeout"}},
		{"workers = 0\nmax_files = 0", []string{"code_index.workers", "code_index.max_files"}},
		// With the failure of another section's key.
		{"workers = 99\n[scm]\nread_timeout = \"0s\"", []string{"code_index.workers", "scm.read_timeout"}},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(noEnv(), dirWith(t, "[code_index]\n"+tc.text+"\n"))
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("Load error = %v, want one naming %s", err, want)
				}
			}
		})
	}
}
