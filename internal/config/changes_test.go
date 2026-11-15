package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestLoad_ReadsTheMergeableDebounceOrItsDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, text string
		want       time.Duration
	}{
		{"empty file", "", 5 * time.Second},
		{"empty [changes]", "[changes]\n", 5 * time.Second},
		{"zero", "[changes]\nmergeable_debounce = \"0s\"\n", 0},
		{"two minutes", "[changes]\nmergeable_debounce = \"2m\"\n", 2 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(noEnv(), dirWith(t, tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Changes.MergeableDebounce != tc.want {
				t.Errorf("MergeableDebounce = %v, want %v", cfg.Changes.MergeableDebounce, tc.want)
			}
		})
	}
}

func TestLoad_RefusesAMergeableDebounceThatIsNotADurationOfZeroOrMore(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`"-1s"`, `"soon"`, `5`, `""`} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(noEnv(), dirWith(t, "[changes]\nmergeable_debounce = "+value+"\n"))
			if err == nil || !strings.Contains(err.Error(), "changes.mergeable_debounce") {
				t.Errorf("Load error = %v, want one naming changes.mergeable_debounce", err)
			}
		})
	}
}
